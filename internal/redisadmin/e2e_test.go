package redisadmin

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
	"pm/internal/redis"
)

// TestE2E installs the real phpRedisAdmin (+ Predis) from GitHub into a
// scratch home and serves it with a real PHP against a real Redis on
// port 26379 — skipped unless REDISADMIN_E2E_PHP names a php binary and
// REDIS_E2E_BIN a folder with redis-server.
func TestE2E(t *testing.T) {
	php, bin := os.Getenv("REDISADMIN_E2E_PHP"), os.Getenv("REDIS_E2E_BIN")
	if php == "" || bin == "" {
		t.Skip("set REDISADMIN_E2E_PHP and REDIS_E2E_BIN to run")
	}
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	old := redis.Port
	redis.Port = 26379
	t.Cleanup(func() { redis.Port = old })
	exe := redis.ServerExe(paths, "9.9.9")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	data, err := os.ReadFile(filepath.Join(bin, "redis-server"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(exe, data, 0o755)
	if err := redis.Start(paths, "9.9.9"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { redis.Stop(paths) })
	c, err := redis.Connect(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Do("SELECT", "2")
	c.Do("SET", "mullion:e2e:key", "hello-from-mullion")
	c.Close()

	v, err := Install(context.Background(), paths, 26379, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed phpRedisAdmin %s (%s)", v, Version(paths))
	if !Installed(paths) {
		t.Fatal("Installed() false after Install")
	}
	if v2, err := Install(context.Background(), paths, 26379, ""); err != nil || v2 != "" {
		t.Errorf("second Install = %q %v, want a no-op", v2, err)
	}

	srv := exec.Command(php, "-n", "-d", "session.save_path="+paths.TmpDir(), "-S", "127.0.0.1:28181", "-t", Dir(paths))
	srv.Stdout, srv.Stderr = io.Discard, io.Discard
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Process.Kill(); srv.Wait() })
	get := func(path string) string {
		var body []byte
		for i := 0; i < 30; i++ {
			resp, err := http.Get("http://127.0.0.1:28181" + path)
			if err == nil {
				body, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				return string(body)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("GET %s: no answer", path)
		return ""
	}
	home := get("/?s=0&d=2")
	if !strings.Contains(home, "phpRedisAdmin") || !strings.Contains(home, "mullion") {
		t.Errorf("index page:\n%.2000s", home)
	}
	overview := get("/overview.php?s=0&d=2")
	if !strings.Contains(overview, "Mullion Redis") || strings.Contains(overview, "Fatal error") {
		t.Errorf("overview page:\n%.2000s", overview)
	}
	view := get("/view.php?s=0&d=2&key=mullion:e2e:key")
	if !strings.Contains(view, "hello-from-mullion") {
		t.Errorf("view page:\n%.2000s", view)
	}
}
