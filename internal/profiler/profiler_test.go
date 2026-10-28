package profiler

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

// A trace as SPX writes it: wt (ns) and zm (bytes) columns, cumulative.
//
//	main 0..100
//	  a 10..60          (self 20: 10..20 and 50..60)
//	    b 20..30
//	    b 30..50
//	      a 35..45      (recursion: a inside a)
//	  b 70..90
const testTrace = `[events]
0 1 0 1000
1 1 10 1000
2 1 20 1100
2 0 30 1200
2 1 30 1200
1 1 35 1200
1 0 45 1300
2 0 50 1300
1 0 60 1500
2 1 70 1500
2 0 90 1400
0 0 100 1400
[functions]
/app/public/index.php
App\A::run
b
`

func parseTest(t *testing.T, trace string) *Profile {
	t.Helper()
	p, err := Parse(strings.NewReader(trace), []string{"wt", "zm"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func find(n *Node, names ...string) *Node {
	for _, name := range names {
		var next *Node
		for _, c := range n.Children {
			if c.Name == name {
				next = c
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

func TestParseAggregatesByStack(t *testing.T) {
	p := parseTest(t, testTrace)
	if p.Events != 12 {
		t.Fatalf("events = %d, want 12", p.Events)
	}
	if p.Root.Total != 100 {
		t.Fatalf("root total = %v, want 100", p.Root.Total)
	}
	main := find(p.Root, "/app/public/index.php")
	if main == nil || main.Total != 100 || main.Self != 30 || main.Calls != 1 {
		t.Fatalf("main = %+v", main)
	}
	a := find(main, "App\\A::run")
	if a == nil || a.Total != 50 || a.Self != 20 || a.Calls != 1 {
		t.Fatalf("main>a = %+v", a)
	}
	// b called twice from a: both calls merge into one frame.
	ab := find(a, "b")
	if ab == nil || ab.Total != 30 || ab.Calls != 2 || ab.Self != 20 {
		t.Fatalf("main>a>b = %+v", ab)
	}
	aba := find(ab, "App\\A::run")
	if aba == nil || aba.Total != 10 || aba.Self != 10 {
		t.Fatalf("main>a>b>a = %+v", aba)
	}
	mb := find(main, "b")
	if mb == nil || mb.Total != 20 || mb.Calls != 1 {
		t.Fatalf("main>b = %+v", mb)
	}
	// Children sorted biggest first.
	if main.Children[0].Name != "App\\A::run" {
		t.Fatalf("children not sorted by time: %s first", main.Children[0].Name)
	}
}

func TestParseTopFunctions(t *testing.T) {
	p := parseTest(t, testTrace)
	got := map[string]Func{}
	for _, f := range p.Funcs {
		got[f.Name] = f
	}
	// a: self 20 (outer) + 10 (inner); inclusive counts the outer call
	// only — the recursive one is inside it.
	if f := got["App\\A::run"]; f.Self != 30 || f.Inclusive != 50 || f.Calls != 2 || f.Memory != 500 {
		t.Fatalf("a = %+v", f)
	}
	// b: self 10 + 10 (20 minus the nested a) + 20 = 40, inclusive 10+20+20.
	if f := got["b"]; f.Self != 40 || f.Inclusive != 50 || f.Calls != 3 {
		t.Fatalf("b = %+v", f)
	}
	if f := got["/app/public/index.php"]; f.Self != 30 || f.Inclusive != 100 || f.Memory != 400 {
		t.Fatalf("main = %+v", f)
	}
	if p.Funcs[0].Name != "b" {
		t.Fatalf("funcs not sorted by self time: %s first", p.Funcs[0].Name)
	}
}

func TestParseGzipAndTruncated(t *testing.T) {
	// Cut before the last two ends: open frames close at the last time.
	cut := strings.Replace(testTrace, "2 0 90 1400\n0 0 100 1400\n", "", 1)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(cut))
	gz.Close()
	p, err := Parse(&buf, []string{"wt", "zm"})
	if err != nil {
		t.Fatal(err)
	}
	main := find(p.Root, "/app/public/index.php")
	if main == nil || main.Total != 70 {
		t.Fatalf("truncated main = %+v", main)
	}
	if mb := find(main, "b"); mb == nil || mb.Total != 0 {
		t.Fatalf("open b = %+v", mb)
	}
}

func TestParseMetricColumns(t *testing.T) {
	// zm first, wt second: the wall-time column follows the metadata.
	p, err := Parse(strings.NewReader("[events]\n0 1 5 0\n0 0 9 40\n[functions]\nmain\n"), []string{"zm", "wt"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Root.Total != 40 {
		t.Fatalf("root = %v, want 40", p.Root.Total)
	}
	if _, err := Parse(strings.NewReader(""), []string{"zm"}); err == nil {
		t.Fatal("a profile without wall time must be refused")
	}
}

func TestPrune(t *testing.T) {
	p := parseTest(t, testTrace)
	p.Prune(0.15) // drops frames under 15ns: main>a>b>a (10)
	main := find(p.Root, "/app/public/index.php")
	if find(main, "App\\A::run", "b", "App\\A::run") != nil {
		t.Fatal("small frame kept")
	}
	if find(main, "b") == nil {
		t.Fatal("20ns frame dropped")
	}
	if p.Pruned != 1 {
		t.Fatalf("pruned = %d, want 1", p.Pruned)
	}
}

const testMeta = `{
  "key": "%s",
  "exec_ts": %d,
  "host_name": "mac",
  "process_pid": 1,
  "process_tid": 0,
  "process_pwd": "\/app",
  "cli": 0,
  "cli_command_line": "n\/a",
  "http_request_uri": "\/api\/users?page=2",
  "http_method": "GET",
  "http_host": "blog.test",
  "custom_metadata_str": null,
  "wall_time_ms": 100,
  "peak_memory_usage": 2048,
  "called_function_count": 3,
  "call_count": 6,
  "recorded_call_count": 6,
  "enabled_metrics": [
    "wt"
    ,"zm"
  ]
}
`

func writeReport(t *testing.T, dir, id string, ts int64, size int) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	meta := strings.Replace(strings.Replace(testMeta, "%s", id, 1), "%d", strconv.FormatInt(ts, 10), 1)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(testTrace))
	gz.Close()
	data := buf.Bytes()
	if size > len(data) {
		data = append(data, make([]byte, size-len(data))...)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".txt.gz"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListLoadDeleteEnforce(t *testing.T) {
	dir := t.TempDir()
	writeReport(t, dir, "spx-full-1-a", 100, 0)
	writeReport(t, dir, "spx-full-2-b", 300, 0)
	writeReport(t, dir, "spx-full-3-c", 200, 0)
	os.WriteFile(filepath.Join(dir, "spx-full-4-d.json"), []byte("{broken"), 0o644)

	metas, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 3 || metas[0].ID != "spx-full-2-b" || metas[2].ID != "spx-full-1-a" {
		t.Fatalf("list = %+v", metas)
	}
	m := metas[0]
	if m.Method != "GET" || m.URI != "/api/users?page=2" || m.Host != "blog.test" || m.WallUs != 100 || m.Calls != 6 || m.Command != "" || m.CLI || m.Bytes == 0 {
		t.Fatalf("meta = %+v", m)
	}

	m, p, err := LoadProfile(dir, "spx-full-3-c")
	if err != nil || p.Root.Total != 100 || m.ID != "spx-full-3-c" {
		t.Fatalf("load: %v %+v", err, m)
	}
	if _, _, err := LoadProfile(dir, "../../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}

	if n, err := Enforce(dir, Limits{MaxReports: 2}); err != nil || n != 1 {
		t.Fatalf("enforce count: %d %v", n, err)
	}
	if metas, _ := List(dir); len(metas) != 2 || metas[1].ID != "spx-full-3-c" {
		t.Fatalf("after enforce: %+v", metas)
	}
	if err := Delete(dir, "spx-full-3-c"); err != nil {
		t.Fatal(err)
	}
	if n, _ := Usage(dir); n != 1 {
		t.Fatalf("usage count = %d", n)
	}
	if n, err := Clear(dir); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if n, _ := Usage(dir); n != 0 {
		t.Fatal("clear left reports")
	}
}

func TestEnforceBytes(t *testing.T) {
	dir := t.TempDir()
	writeReport(t, dir, "spx-full-1-a", 100, 5000)
	writeReport(t, dir, "spx-full-2-b", 200, 5000)
	writeReport(t, dir, "spx-full-3-c", 300, 5000)
	if n, err := Enforce(dir, Limits{MaxBytes: 12000}); err != nil || n != 1 {
		t.Fatalf("enforce bytes: %d %v", n, err)
	}
	metas, _ := List(dir)
	if len(metas) != 2 || metas[1].ID != "spx-full-2-b" {
		t.Fatalf("kept %+v", metas)
	}
	// The newest report is kept even when it alone is over the limit.
	if n, _ := Enforce(dir, Limits{MaxBytes: 10}); n != 1 {
		t.Fatalf("enforce tiny limit removed %d", n)
	}
	if metas, _ := List(dir); len(metas) != 1 || metas[0].ID != "spx-full-3-c" {
		t.Fatalf("kept %+v", metas)
	}
}

func TestAdminValue(t *testing.T) {
	paths := pmdir.Paths{Home: "/Users/me/.mullion"}
	pool := Pool{SpxLoaded: true, Profiled: true}
	s := &Site{Mode: ModeBrowser, Key: "k1"}

	got := AdminValue(paths, "blog", s, pool, SiteOpts{})
	want := strings.Join([]string{
		`spx.http_enabled=1`,
		`spx.http_key="k1"`,
		`spx.http_ip_whitelist="127.0.0.1,::1"`,
		`spx.data_dir="/Users/me/.mullion/profiler/blog/reports"`,
		`spx.http_profiling_builtins=0`,
		`spx.http_profiling_enabled=0`,
		`spx.http_profiling_auto_start=1`,
	}, "\n")
	if got != want {
		t.Fatalf("browser:\n%s\nwant:\n%s", got, want)
	}

	s.Mode, s.Builtins = ModeAll, true
	got = AdminValue(paths, "blog", s, pool, SiteOpts{})
	if !strings.Contains(got, "spx.http_profiling_builtins=1\nspx.http_profiling_enabled=1\nspx.http_profiling_auto_start=1") || strings.Contains(got, "auto_prepend_file") {
		t.Fatalf("all:\n%s", got)
	}

	s.Mode = ModeNext
	pool.Gated = true
	got = AdminValue(paths, "blog", s, pool, SiteOpts{})
	if !strings.HasSuffix(got, "spx.http_profiling_enabled=1\nspx.http_profiling_auto_start=0\nauto_prepend_file=\"/Users/me/.mullion/profiler/gate.php\"") {
		t.Fatalf("next:\n%s", got)
	}

	// Other sites of the pool switch SPX off and get their prepend back.
	pool.IniPrepend = "/srv/prepend.php"
	got = AdminValue(paths, "shop", nil, pool, SiteOpts{})
	if got != "spx.http_enabled=0\nspx.http_profiling_enabled=0\nauto_prepend_file=\"/srv/prepend.php\"" {
		t.Fatalf("other site:\n%s", got)
	}
	// …unless the watcher sets its own in PHP_VALUE.
	got = AdminValue(paths, "shop", nil, pool, SiteOpts{WatchPrepend: "/w.php"})
	if got != "spx.http_enabled=0\nspx.http_profiling_enabled=0" {
		t.Fatalf("watched other site:\n%s", got)
	}
	// Nothing when no site of the pool is profiled, or SPX isn't loaded.
	if v := AdminValue(paths, "shop", nil, Pool{SpxLoaded: true}, SiteOpts{}); v != "" {
		t.Fatalf("unprofiled pool: %q", v)
	}
	if v := AdminValue(paths, "blog", s, Pool{Profiled: true}, SiteOpts{}); v != "" {
		t.Fatalf("no spx.so: %q", v)
	}
}

func TestEnvGateParams(t *testing.T) {
	paths := pmdir.Paths{Home: "/h"}
	s := &Site{Mode: ModePath, Path: "/api", Key: "k"}
	pool := Pool{SpxLoaded: true, Profiled: true, Gated: true, IniPrepend: "/ini.php"}
	env := Env(paths, "blog", s, pool, SiteOpts{WatchPrepend: "/h/watch/mullion-watch.php"})
	if len(env) != 3 || env[0].Name != "PHP_ADMIN_VALUE" ||
		env[1] != (EnvVar{"MULLION_SPX", "/h/profiler/blog/gate.json"}) ||
		env[2] != (EnvVar{"MULLION_SPX_CHAIN", "/h/watch/mullion-watch.php"}) {
		t.Fatalf("env = %+v", env)
	}
	// Not watched: chain to php.ini's prepend.
	env = Env(paths, "blog", s, pool, SiteOpts{})
	if env[2].Value != "/ini.php" {
		t.Fatalf("chain = %+v", env)
	}
	// Browser mode: no gate params.
	if env := Env(paths, "blog", &Site{Mode: ModeBrowser, Key: "k"}, pool, SiteOpts{}); len(env) != 1 {
		t.Fatalf("browser env = %+v", env)
	}
	if env := Env(paths, "blog", s, Pool{}, SiteOpts{}); env != nil {
		t.Fatalf("no spx: %+v", env)
	}
}

func TestValidateAndState(t *testing.T) {
	s := Site{Mode: "NEXT"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Mode != ModeNext || s.Count != 10 || len(s.Key) != 32 || s.Since == 0 {
		t.Fatalf("validated = %+v", s)
	}
	p := Site{Mode: ModePath, Path: "api/*"}
	if err := p.Validate(); err != nil || p.Path != "/api/*" {
		t.Fatalf("path: %v %+v", err, p)
	}
	if err := (&Site{Mode: ModePath}).Validate(); err == nil {
		t.Fatal("path mode without a path accepted")
	}
	if err := (&Site{Mode: "sometimes"}).Validate(); err == nil {
		t.Fatal("unknown mode accepted")
	}

	paths := pmdir.Paths{Home: t.TempDir()}
	st, err := LoadState(paths)
	if err != nil || len(st.Sites) != 0 || st.Limits != DefaultLimits {
		t.Fatalf("empty state: %v %+v", err, st)
	}
	st.Sites["blog"] = &s
	if err := st.Save(paths); err != nil {
		t.Fatal(err)
	}
	st2, err := LoadState(paths)
	if err != nil || st2.Get("blog") == nil || st2.Get("blog").Key != s.Key || st2.Get("shop") != nil {
		t.Fatalf("reloaded: %v %+v", err, st2)
	}
}

func TestPrepareGate(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	s := &Site{Mode: ModeNext, Count: 3, Path: "/api"}
	if err := Prepare(paths, "blog", s, true); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(GatePath(paths)); err != nil || string(data) != gateSource {
		t.Fatalf("gate.php not written: %v", err)
	}
	g, ok := ReadGate(paths, "blog")
	if !ok || g.Remaining != 3 || g.Path != "/api" {
		t.Fatalf("gate = %+v %v", g, ok)
	}
	// The gate counts down; regenerating the Caddyfile must not reset it.
	os.WriteFile(gateConfigPath(paths, "blog"), []byte(`{"remaining":1,"path":"/api","captured":2}`), 0o644)
	if err := Prepare(paths, "blog", s, false); err != nil {
		t.Fatal(err)
	}
	if g, _ := ReadGate(paths, "blog"); g.Remaining != 1 || g.Captured != 2 {
		t.Fatalf("gate reset: %+v", g)
	}
	if err := Prepare(paths, "blog", s, true); err != nil {
		t.Fatal(err)
	}
	if g, _ := ReadGate(paths, "blog"); g.Remaining != 3 || g.Captured != 0 {
		t.Fatalf("gate not reset: %+v", g)
	}
	// Browser mode drops the gate state; the reports dir exists.
	if err := Prepare(paths, "blog", &Site{Mode: ModeBrowser}, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadGate(paths, "blog"); ok {
		t.Fatal("gate.json kept in browser mode")
	}
	if _, err := os.Stat(ReportsDir(paths, "blog")); err != nil {
		t.Fatal(err)
	}
	// ModePath has no count limit.
	if g := gateStateFor(&Site{Mode: ModePath, Path: "/x"}); g.Remaining != -1 {
		t.Fatalf("path gate = %+v", g)
	}
}
