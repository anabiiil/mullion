package redis

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a tiny in-process RESP server: a string/list/hash
// keyspace per database and just the commands the client uses.
type fakeServer struct {
	ln      net.Listener
	mu      sync.Mutex
	dbs     map[int]map[string]any
	ttl     map[string]int64
	log     []string // every command received, space-joined
	monitor []string // lines MONITOR streams
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, dbs: map[int]map[string]any{0: {}}, ttl: map[string]int64{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) set(db int, key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dbs[db] == nil {
		s.dbs[db] = map[string]any{}
	}
	s.dbs[db][key] = v
}

func readCmd(r *bufio.Reader) ([]string, error) {
	v, err := readValue(r)
	if err != nil {
		return nil, err
	}
	return v.Strings(), nil
}

func bulk(s string) string { return "$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n" }
func intr(n int) string    { return ":" + strconv.Itoa(n) + "\r\n" }
func arr(items ...string) string {
	return "*" + strconv.Itoa(len(items)) + "\r\n" + strings.Join(items, "")
}
func bulks(ss []string) string {
	out := make([]string, len(ss))
	for i, x := range ss {
		out[i] = bulk(x)
	}
	return arr(out...)
}

func (s *fakeServer) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	db := 0
	var snapshot []string // SCAN's view, taken when a scan starts
	for {
		args, err := readCmd(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.log = append(s.log, strings.Join(args, " "))
		keys := s.dbs[db]
		if keys == nil {
			keys = map[string]any{}
			s.dbs[db] = keys
		}
		var out string
		switch strings.ToUpper(args[0]) {
		case "PING":
			out = "+PONG\r\n"
		case "AUTH":
			if args[1] == "secret" {
				out = "+OK\r\n"
			} else {
				out = "-WRONGPASS invalid username-password pair\r\n"
			}
		case "SELECT":
			db, _ = strconv.Atoi(args[1])
			out = "+OK\r\n"
		case "INFO":
			out = bulk("# Server\r\nredis_version:8.10.2\r\n\r\n# Stats\r\ninstantaneous_ops_per_sec:12\r\nkeyspace_hits:30\r\nkeyspace_misses:10\r\n\r\n# Keyspace\r\ndb0:keys=3,expires=1,avg_ttl=5000\r\ndb2:keys=7,expires=0,avg_ttl=0\r\n")
		case "SCAN":
			// Two pages: cursor 0 -> 7 -> 0, splitting the sorted matches.
			match := "*"
			for i := 2; i+1 < len(args); i += 2 {
				if strings.EqualFold(args[i], "MATCH") {
					match = args[i+1]
				}
			}
			if args[1] == "0" {
				snapshot = nil
				for k := range keys {
					if ok, _ := path.Match(match, k); ok {
						snapshot = append(snapshot, k)
					}
				}
				sort.Strings(snapshot)
			}
			all := snapshot
			half := len(all) / 2
			if args[1] == "0" {
				out = arr(bulk("7"), bulks(all[:half]))
			} else {
				out = arr(bulk("0"), bulks(all[half:]))
			}
		case "TYPE":
			switch keys[args[1]].(type) {
			case string:
				out = "+string\r\n"
			case []string:
				out = "+list\r\n"
			case map[string]string:
				out = "+hash\r\n"
			default:
				out = "+none\r\n"
			}
		case "PTTL":
			if _, ok := keys[args[1]]; !ok {
				out = intr(-2)
			} else if t, ok := s.ttl[args[1]]; ok {
				out = intr(int(t))
			} else {
				out = intr(-1)
			}
		case "MEMORY":
			out = intr(56)
		case "STRLEN":
			out = intr(len(keys[args[1]].(string)))
		case "GETRANGE":
			v := keys[args[1]].(string)
			end, _ := strconv.Atoi(args[3])
			if end+1 < len(v) {
				v = v[:end+1]
			}
			out = bulk(v)
		case "LLEN":
			out = intr(len(keys[args[1]].([]string)))
		case "LRANGE":
			l := keys[args[1]].([]string)
			end, _ := strconv.Atoi(args[3])
			if end+1 < len(l) {
				l = l[:end+1]
			}
			out = bulks(l)
		case "HLEN":
			out = intr(len(keys[args[1]].(map[string]string)))
		case "HSCAN":
			var flat []string
			for k, v := range keys[args[1]].(map[string]string) {
				flat = append(flat, k, v)
			}
			out = arr(bulk("0"), bulks(flat))
		case "UNLINK":
			n := 0
			for _, k := range args[1:] {
				if _, ok := keys[k]; ok {
					delete(keys, k)
					n++
				}
			}
			out = intr(n)
		case "FLUSHDB":
			s.dbs[db] = map[string]any{}
			out = "+OK\r\n"
		case "DBSIZE":
			out = intr(len(keys))
		case "GET":
			if v, ok := keys[args[1]].(string); ok {
				out = bulk(v)
			} else {
				out = "$-1\r\n"
			}
		case "MONITOR":
			lines := s.monitor
			s.mu.Unlock()
			c.Write([]byte("+OK\r\n"))
			for _, l := range lines {
				c.Write([]byte("+" + l + "\r\n"))
			}
			// Then stay quiet until the client goes away.
			buf := make([]byte, 1)
			c.Read(buf)
			return
		default:
			out = "-ERR unknown command '" + args[0] + "'\r\n"
		}
		s.mu.Unlock()
		c.Write([]byte(out))
	}
}

func dialFake(t *testing.T, s *fakeServer) *Client {
	t.Helper()
	c, err := Dial(s.addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRESPBasics(t *testing.T) {
	s := newFakeServer(t)
	c := dialFake(t, s)
	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := c.Auth("nope"); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("auth error = %v", err)
	}
	if _, ok := err2Error(c.Auth("nope")); !ok {
		t.Error("an error reply should be a redis.Error")
	}
	if err := c.Auth("secret"); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Do("GET", "missing"); err != nil || !v.Nil {
		t.Errorf("GET missing = %+v %v", v, err)
	}
	if _, err := c.Do("NOPE"); err == nil {
		t.Error("unknown command should fail")
	}
	// Binary-safe arguments go through intact.
	s.set(0, "a b\r\nc", "x")
	if v, err := c.Do("GET", "a b\r\nc"); err != nil || v.Str != "x" {
		t.Errorf("binary key: %+v %v", v, err)
	}
}

func err2Error(err error) (Error, bool) {
	e, ok := err.(Error)
	return e, ok
}

func TestRESPInfoAndKeyspace(t *testing.T) {
	c := dialFake(t, newFakeServer(t))
	info, err := c.Info("")
	if err != nil {
		t.Fatal(err)
	}
	if info["redis_version"] != "8.10.2" || info["keyspace_hits"] != "30" || info["instantaneous_ops_per_sec"] != "12" {
		t.Errorf("info = %v", info)
	}
	ks := ParseKeyspace(info)
	if len(ks) != 2 || ks[0] != (KeyspaceDB{DB: 0, Keys: 3, Expires: 1, AvgTTL: 5000}) || ks[1].DB != 2 || ks[1].Keys != 7 {
		t.Errorf("keyspace = %+v", ks)
	}
}

func TestRESPScanAndDelete(t *testing.T) {
	s := newFakeServer(t)
	for i := 0; i < 6; i++ {
		s.set(0, fmt.Sprintf("app_database_k%d", i), "v")
	}
	s.set(0, "other_key", "v")
	c := dialFake(t, s)
	keys, more, err := c.ScanAll("app_database_*", 0)
	if err != nil || more || len(keys) != 6 {
		t.Fatalf("ScanAll = %v %v %v", keys, more, err)
	}
	keys, more, err = c.ScanAll("app_database_*", 2)
	if err != nil || !more || len(keys) != 2 {
		t.Fatalf("ScanAll(max 2) = %v %v %v", keys, more, err)
	}
	n, err := c.DeleteMatching("app_database_*")
	if err != nil || n != 6 {
		t.Fatalf("DeleteMatching = %d %v", n, err)
	}
	if left, _ := c.DBSize(); left != 1 {
		t.Errorf("%d keys left, want only other_key", left)
	}
	if _, err := c.DeleteMatching("*"); err == nil {
		t.Error("deleting everything by pattern must be refused")
	}
	// SCAN carries MATCH and COUNT; nothing ever sends KEYS.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.log {
		if strings.HasPrefix(strings.ToUpper(l), "KEYS") {
			t.Errorf("KEYS was sent: %q", l)
		}
	}
	if !strings.Contains(strings.Join(s.log, "\n"), "SCAN 0 MATCH app_database_* COUNT 1000") {
		t.Errorf("SCAN not sent as expected: %v", s.log)
	}
}

func TestRESPPreview(t *testing.T) {
	s := newFakeServer(t)
	s.set(0, "str", strings.Repeat("x", 100))
	s.set(0, "list", []string{"a", "b", "c", "d"})
	s.set(0, "hash", map[string]string{"f2": "2", "f1": "1"})
	s.ttl["str"] = 9000
	c := dialFake(t, s)

	p, err := c.GetPreview("str", 10, 16)
	if err != nil || p.Type != "string" || p.Size != 100 || len(p.Value) != 16 || !p.Truncated || p.TTL != 9000 || p.Memory != 56 {
		t.Errorf("string preview = %+v %v", p, err)
	}
	p, err = c.GetPreview("list", 2, 0)
	if err != nil || p.Size != 4 || len(p.Items) != 2 || !p.Truncated || p.TTL != -1 {
		t.Errorf("list preview = %+v %v", p, err)
	}
	p, err = c.GetPreview("hash", 10, 0)
	if err != nil || len(p.Fields) != 2 || p.Fields[0].Name != "f1" || p.Truncated {
		t.Errorf("hash preview = %+v %v", p, err)
	}
	if _, err := c.GetPreview("gone", 10, 0); err == nil {
		t.Error("a missing key should fail")
	}
}

func TestParseMonitorLine(t *testing.T) {
	ev, err := ParseMonitorLine(`1700000000.123456 [3 127.0.0.1:52345] "SET" "app_database_k" "a \"q\" \\ \x41\n"`)
	if err != nil {
		t.Fatal(err)
	}
	if ev.DB != 3 || ev.Addr != "127.0.0.1:52345" || ev.Time != 1700000000.123456 {
		t.Errorf("event = %+v", ev)
	}
	if len(ev.Args) != 3 || ev.Args[0] != "SET" || ev.Args[2] != "a \"q\" \\ A\n" {
		t.Errorf("args = %q", ev.Args)
	}
	ev, err = ParseMonitorLine(`1700000000.000001 [0 lua] "get" "k"`)
	if err != nil || ev.Addr != "lua" || len(ev.Args) != 2 {
		t.Errorf("lua line = %+v %v", ev, err)
	}
	for _, bad := range []string{"", "OK", "x [0 a] \"a\"", `1.0 [z a] "a"`, `1.0 [0 a] "unterminated`} {
		if _, err := ParseMonitorLine(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestRESPMonitor(t *testing.T) {
	s := newFakeServer(t)
	s.monitor = []string{
		`1700000000.1 [0 127.0.0.1:1] "SET" "app_database_a" "1"`,
		`garbage`,
		`1700000000.2 [1 127.0.0.1:1] "GET" "app_database_b"`,
		`1700000000.3 [0 127.0.0.1:1] "GET" "x"`,
	}
	c := dialFake(t, s)
	var got []MonitorEvent
	err := c.Monitor(context.Background(), func(ev MonitorEvent) bool {
		got = append(got, ev)
		return len(got) < 3
	})
	if err != nil || len(got) != 3 || got[1].DB != 1 || got[2].Args[1] != "x" {
		t.Fatalf("monitor = %+v %v", got, err)
	}

	// A cancelled context ends a quiet stream.
	s.monitor = nil
	c2 := dialFake(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c2.Monitor(ctx, func(MonitorEvent) bool { return true }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("cancelled monitor returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Monitor did not return after its context ended")
	}
}

func TestPanelAddrs(t *testing.T) {
	c := dialFake(t, newFakeServer(t))
	local := c.conn.LocalAddr().String()
	if !IsPanelAddr(local) {
		t.Error("the client's own address should be recognized")
	}
	c.Close()
	if IsPanelAddr(local) {
		t.Error("a closed client's address should be forgotten")
	}
}

func TestFormatArgsAndGlob(t *testing.T) {
	if got := FormatArgs([]string{"set", "k", "two words", strings.Repeat("v", 20)}, 10); got != `SET k "two words" vvvvvvvvvv…` {
		t.Errorf("FormatArgs = %q", got)
	}
	if got := GlobEscape("a*b?[c]"); got != `a\*b\?\[c\]` {
		t.Errorf("GlobEscape = %q", got)
	}
}
