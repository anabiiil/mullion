package redis

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// TestE2E is a manual end-to-end check against a REAL redis-server in a
// scratch home — skipped unless REDIS_E2E_BIN names a folder holding
// redis-server and redis-cli (e.g. a source tree's src/ after
// `make -C src redis-server redis-cli`). It runs on port 26379, so it
// never touches a Redis on the default port.
//
// With REDIS_E2E_BUILD=1 instead, it runs the real install recipe
// (download from download.redis.io, checksum, build with the system
// compiler) into the scratch home first.
func TestE2E(t *testing.T) {
	src := os.Getenv("REDIS_E2E_BIN")
	build := os.Getenv("REDIS_E2E_BUILD") == "1"
	if src == "" && !build {
		t.Skip("set REDIS_E2E_BIN to a folder with redis-server/redis-cli (or REDIS_E2E_BUILD=1) to run")
	}
	home := t.TempDir()
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	ver := "9.9.9"
	if build {
		start := time.Now()
		v, err := Install(context.Background(), paths, func(s string) { t.Log(s) })
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("installed %s in %s", v, time.Since(start).Round(time.Second))
		ver = v
	} else {
		for _, name := range []string{"redis-server", "redis-cli"} {
			dst := filepath.Join(versionDir(paths, ver), "bin", pmdir.ExeName(name))
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := copyFile(filepath.Join(src, pmdir.ExeName(name)), dst, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	oldPort := Port
	Port = 26379
	t.Cleanup(func() { Port = oldPort })
	if Running() {
		t.Fatal("port 26379 is busy")
	}
	if err := Start(paths, ver); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Stop(paths) })
	if !Owned(paths) {
		t.Fatal("Owned() should see the server")
	}
	if err := Start(paths, ver); err != nil { // idempotent
		t.Fatal(err)
	}

	c, err := Connect(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info("")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("server: redis_version=%s maxmemory_human=%s maxmemory_policy=%s", info["redis_version"], info["maxmemory_human"], info["maxmemory_policy"])
	if info["maxmemory_policy"] != "noeviction" || info["maxmemory_human"] != "512.00M" {
		t.Errorf("config not applied: %v %v", info["maxmemory_policy"], info["maxmemory_human"])
	}

	// A Laravel-ish keyspace across two databases.
	mustDo := func(args ...string) Value {
		t.Helper()
		v, err := c.Do(args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return v
	}
	mustDo("SELECT", "1")
	for i := 0; i < 50; i++ {
		mustDo("SET", "shop_database_shop_cache_k"+string(rune('a'+i%26))+strings.Repeat("x", i), "v", "EX", "3600")
	}
	mustDo("SET", "other_key", "keep")
	mustDo("SELECT", "0")
	mustDo("RPUSH", "shop_database_queues:default", "job1", "job2", "job3")
	mustDo("HSET", "shop_database_h", "f1", "1", "f2", "2")
	mustDo("ZADD", "shop_database_z", "1", "a", "2", "b")
	mustDo("SADD", "shop_database_s", "m1", "m2")
	mustDo("XADD", "shop_database_x", "*", "f", "v")

	ks := ParseKeyspace(must(c.Info("keyspace")))
	t.Logf("keyspace: %+v", ks)
	if len(ks) != 2 || ks[0].Keys != 5 || ks[1].Keys != 51 || ks[1].Expires != 50 {
		t.Errorf("keyspace = %+v", ks)
	}
	for _, key := range []string{"shop_database_queues:default", "shop_database_h", "shop_database_z", "shop_database_s", "shop_database_x"} {
		p, err := c.GetPreview(key, 2, 0)
		if err != nil {
			t.Errorf("preview %s: %v", key, err)
			continue
		}
		t.Logf("preview %s: type=%s size=%d mem=%d ttl=%d items=%v fields=%v truncated=%v", key, p.Type, p.Size, p.Memory, p.TTL, p.Items, p.Fields, p.Truncated)
	}

	// MONITOR sees another client's commands; our own address is known.
	mon, err := Connect(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan MonitorEvent, 10)
	go func() {
		mon.Monitor(ctx, func(ev MonitorEvent) bool {
			got <- ev
			return true
		})
		mon.Close()
	}()
	time.Sleep(200 * time.Millisecond)
	mustDo("EXISTS", "shop_database_h")
	select {
	case ev := <-got:
		t.Logf("monitor: db=%d addr=%s args=%q panel=%v", ev.DB, ev.Addr, ev.Args, IsPanelAddr(ev.Addr))
		if !IsPanelAddr(ev.Addr) {
			t.Error("our own connection should be recognized in MONITOR output")
		}
	case <-time.After(3 * time.Second):
		t.Error("MONITOR reported nothing")
	}
	cancel()

	// Prefix delete (cache only) leaves the other key alone.
	mustDo("SELECT", "1")
	n, err := c.DeleteMatching(GlobEscape("shop_database_shop_cache_") + "*")
	if err != nil || n != 50 {
		t.Errorf("DeleteMatching = %d %v", n, err)
	}
	if left, _ := c.DBSize(); left != 1 {
		t.Errorf("db1 has %d keys, want 1", left)
	}

	// Stop saves; a restart loads the snapshot.
	if err := Stop(paths); err != nil {
		t.Fatal(err)
	}
	if Owned(paths) || Running() {
		t.Fatal("still running after Stop")
	}
	if st, err := os.Stat(DumpFile(paths)); err != nil || st.Size() == 0 {
		t.Fatalf("no dump.rdb after a clean stop: %v", err)
	}
	if err := Start(paths, ver); err != nil {
		t.Fatal(err)
	}
	c2, err := Connect(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if n, _ := c2.DBSize(); n != 5 {
		t.Errorf("after restart db0 has %d keys, want 5", n)
	}
	if runtime.GOOS != "windows" {
		if b, _ := os.ReadFile(LogFile(paths)); !strings.Contains(string(b), "Ready to accept connections") {
			t.Errorf("log lacks the ready line:\n%s", b)
		}
	}
}

func must(m map[string]string, err error) map[string]string {
	if err != nil {
		panic(err)
	}
	return m
}
