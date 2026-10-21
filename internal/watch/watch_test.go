package watch

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

const time10s = 10 * time.Second

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTailerFollowsAppendsPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, path, "old1\nold2\nold3\n")
	tl := NewTailer(path)
	if got := tl.Backlog(2, 1<<20); !reflect.DeepEqual(got, []string{"old2", "old3"}) {
		t.Fatalf("backlog = %q", got)
	}
	if got := tl.Poll(); got != nil {
		t.Fatalf("nothing new yet, got %q", got)
	}
	appendFile(t, path, "a\nb")
	if got := tl.Poll(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("partial line must wait: %q", got)
	}
	appendFile(t, path, "c\r\n")
	if got := tl.Poll(); !reflect.DeepEqual(got, []string{"bc"}) {
		t.Fatalf("completed line = %q", got)
	}
}

func TestTailerTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	appendFile(t, path, "one\ntwo\n")
	tl := NewTailer(path)
	tl.Backlog(10, 1<<20)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "x\n")
	if got := tl.Poll(); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("after truncation = %q", got)
	}
}

func TestTailerRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	appendFile(t, path, "first\n")
	tl := NewTailer(path)
	tl.Backlog(10, 1<<20)
	// The writer renames the full file away and starts a new one that is
	// already LONGER than what was read — only identity catches that.
	if err := os.Rename(path, filepath.Join(dir, "events.1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := tl.Poll(); got != nil {
		t.Fatalf("missing file must read nothing: %q", got)
	}
	appendFile(t, path, "new-a\nnew-b\n")
	if got := tl.Poll(); !reflect.DeepEqual(got, []string{"new-a", "new-b"}) {
		t.Fatalf("after rotation = %q", got)
	}
	// Rotation between two polls (no "missing" moment in between).
	if err := os.Rename(path, filepath.Join(dir, "events.1.jsonl")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "fresh-file-line-1\nfresh-file-line-2\n")
	if got := tl.Poll(); !reflect.DeepEqual(got, []string{"fresh-file-line-1", "fresh-file-line-2"}) {
		t.Fatalf("rotation by identity = %q", got)
	}
}

func TestBacklogSkipsCutFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	appendFile(t, path, "aaaaaaaaaa\nbb\ncc\n")
	tl := NewTailer(path)
	// A window that starts mid-line drops that line…
	if got := tl.Backlog(10, 5); !reflect.DeepEqual(got, []string{"cc"}) {
		t.Fatalf("cut line must be dropped: %q", got)
	}
	// …one that starts exactly on a line keeps it.
	if got := tl.Backlog(10, 6); !reflect.DeepEqual(got, []string{"bb", "cc"}) {
		t.Fatalf("cut line must be dropped: %q", got)
	}
}

const laravelLog = `[2026-10-21 19:00:37] local.WARNING: watch warning {"k":1}
[2026-10-21 19:00:37] local.ERROR: SQLSTATE[HY000]: no such table: posts (Connection: sqlite, SQL: select * from "posts") {"userId":1,"exception":"[object] (Illuminate\\Database\\QueryException(code: HY000): SQLSTATE[HY000]: no such table: posts (Connection: sqlite, SQL: select * from \"posts\") at /app/vendor/laravel/framework/src/Illuminate/Database/Connection.php:825)
[stacktrace]
#0 /app/vendor/laravel/framework/src/Illuminate/Database/Connection.php(779): Illuminate\\Database\\Connection->runQueryCallback()
#1 [internal function]: App\\Http\\Controllers\\PostController->index()
#2 /app/routes/web.php(29): Illuminate\\Support\\Facades\\Facade::__callStatic()
#3 {main}

[previous exception] [object] (PDOException(code: HY000): no such table at /app/x.php:1)
"}
[2026-10-21 19:01:26] local.INFO: closure job ran
[2026-10-21 19:01:27] production.DEBUG: multi
line
message [] []
`

func TestMonologParser(t *testing.T) {
	var p MonologParser
	var got []*LogEntry
	for _, l := range strings.Split(strings.TrimSuffix(laravelLog, "\n"), "\n") {
		if e := p.Feed(l); e != nil {
			got = append(got, e)
		}
	}
	if e := p.Flush(); e != nil {
		got = append(got, e)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %d", len(got))
	}
	if got[0].Level != "WARNING" || got[0].Message != `watch warning {"k":1}` || got[0].Exception != "" {
		t.Fatalf("first = %+v", got[0])
	}
	ex := got[1]
	if ex.Exception != `Illuminate\Database\QueryException` || ex.Line != 825 ||
		ex.File != "/app/vendor/laravel/framework/src/Illuminate/Database/Connection.php" {
		t.Fatalf("exception = %q %s:%d", ex.Exception, ex.File, ex.Line)
	}
	if !strings.HasPrefix(ex.Message, "SQLSTATE[HY000]: no such table: posts") {
		t.Fatalf("message = %q", ex.Message)
	}
	if len(ex.Trace) != 3 || ex.Trace[2].File != "/app/routes/web.php" || ex.Trace[2].Line != 29 ||
		ex.Trace[1].File != "" || !strings.Contains(ex.Trace[0].Call, `Illuminate\Database\Connection->runQueryCallback`) {
		t.Fatalf("trace = %+v", ex.Trace)
	}
	if got[2].Message != "closure job ran" {
		t.Fatalf("third = %q", got[2].Message)
	}
	if got[3].Env != "production" || got[3].Level != "DEBUG" || got[3].Message != "multi\nline\nmessage" {
		t.Fatalf("multi-line = %+v", got[3])
	}
	it := ex.Item("laravel.log")
	if it.Type() != "exception" || it["group"] != `Illuminate\Database\QueryException@/app/vendor/laravel/framework/src/Illuminate/Database/Connection.php:825` {
		t.Fatalf("item = %v", it)
	}
	if got[0].Time.Unix() != 1792609237 {
		t.Fatalf("time = %v", got[0].Time)
	}
}

func TestParseCaddyLine(t *testing.T) {
	line := `{"level":"info","ts":1792609216.155,"logger":"http.log.access.log0","msg":"handled request","request":{"remote_ip":"127.0.0.1","remote_port":"5555","client_ip":"127.0.0.1","proto":"HTTP/2.0","method":"POST","host":"blog.test","uri":"/login?next=%2Fhome&token=abc","headers":{"Accept":["text/html"],"Cookie":["REDACTED"],"X-Api-Key":["k-123"]}},"bytes_read":12,"user_id":"","duration":0.61234,"size":1587,"status":302,"resp_headers":{"Set-Cookie":["s=1"],"Location":["/home"]},"mullion_rid":"6f1c-uuid"}`
	it, ok := ParseCaddyLine(line)
	if !ok {
		t.Fatal("not parsed")
	}
	if it["method"] != "POST" || it["path"] != "/login" || it["status"] != 302 || it["ms"] != 612.34 || it["size"] != int64(1587) || it.RID() != "6f1c-uuid" {
		t.Fatalf("item = %v", it)
	}
	h := it["headers"].(map[string][]string)
	if h["X-Api-Key"][0] != Redacted || h["Accept"][0] != "text/html" {
		t.Fatalf("headers = %v", h)
	}
	q := it["query"].(map[string][]string)
	if q["next"][0] != "/home" || q["token"][0] != Redacted {
		t.Fatalf("query = %v", q)
	}
	if rh := it["respHeaders"].(map[string][]string); rh["Set-Cookie"][0] != Redacted {
		t.Fatalf("resp headers = %v", rh)
	}
	if it["static"] != false {
		t.Fatal("/login is not static")
	}
	st, _ := ParseCaddyLine(`{"ts":1,"msg":"handled request","request":{"method":"GET","uri":"/build/assets/app-x1.js"},"status":200}`)
	if st["static"] != true {
		t.Fatal("/build/… is static")
	}
	for _, bad := range []string{"", "not json", `{"level":"error","msg":"dial tcp: refused"}`} {
		if _, ok := ParseCaddyLine(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
}

func TestNormalizeAndAnalyze(t *testing.T) {
	if got := NormalizeSQL("SELECT * FROM \"users\"  WHERE id = 5 AND name = 'o''brien' AND x IN (?, ?,?)"); got != `select * from "users" where id = ? and name = ? and x in (?)` {
		t.Fatalf("normalize = %q", got)
	}
	var qs []Query
	for i := 0; i < 6; i++ {
		qs = append(qs, Query{SQL: `select * from "posts" where "user_id" = ?`, Bindings: []any{float64(i)}, Ms: 1, File: "app/Http/Controllers/Feed.php", Line: 12})
	}
	// Same shape from another line: a different group (4 < 5).
	for i := 0; i < 4; i++ {
		qs = append(qs, Query{SQL: `select * from "posts" where "user_id" = ?`, Bindings: []any{float64(i)}, Ms: 1, File: "app/Other.php", Line: 3})
	}
	qs = append(qs,
		Query{SQL: "select * from settings", Ms: 150, File: "a.php", Line: 1},
		Query{SQL: "select * from settings", Ms: 2, File: "b.php", Line: 2},
	)
	a := Analyze(qs, 5, 100)
	if a.Queries != 12 || a.Slow != 1 || a.Ms != 162 {
		t.Fatalf("summary = %+v", a)
	}
	if len(a.Issues) < 2 || a.Issues[0].Kind != "n+1" || a.Issues[0].Count != 6 || a.Issues[0].File != "app/Http/Controllers/Feed.php" || a.Issues[0].Line != 12 {
		t.Fatalf("issues = %+v", a.Issues)
	}
	for _, is := range a.Issues {
		if is.Kind == "n+1" && is.File == "app/Other.php" {
			t.Fatalf("4 repeats are under the threshold: %+v", is)
		}
	}
	// Duplicates: settings twice (same SQL, no bindings) and posts with
	// binding 0..3 repeated across the two lines (4 extra copies).
	if a.Duplicates != 5 {
		t.Fatalf("duplicates = %d (%+v)", a.Duplicates, a.Issues)
	}
}

func TestRedaction(t *testing.T) {
	cols := PlaceholderColumns(`update "users" set "password" = ?, "remember_token" = ?, "name" = ? where "id" = ? and email like ?`)
	if !reflect.DeepEqual(cols, []string{"password", "remember_token", "name", "id", "email"}) {
		t.Fatalf("cols = %q", cols)
	}
	got := RedactBindings(`update "users" set "password" = ?, "name" = ? where "id" = ?`, []any{"hash", "Ann", 1.0})
	if !reflect.DeepEqual(got, []any{Redacted, "Ann", 1.0}) {
		t.Fatalf("update = %v", got)
	}
	got = RedactBindings(`insert into "users" ("name", "email", "password", "api_key") values (?, ?, ?, ?), (?, ?, ?, ?)`,
		[]any{"a", "a@x", "p1", "k1", "b", "b@x", "p2", "k2"})
	if !reflect.DeepEqual(got, []any{"a", "a@x", Redacted, Redacted, "b", "b@x", Redacted, Redacted}) {
		t.Fatalf("insert = %v", got)
	}
	// A '?' inside a string literal is not a placeholder.
	got = RedactBindings(`select * from t where note = 'why?' and "token" = ?`, []any{"tok"})
	if !reflect.DeepEqual(got, []any{Redacted}) {
		t.Fatalf("quoted ? = %v", got)
	}
	plain := []any{"x"}
	if got := RedactBindings(`select * from users where id = ?`, plain); !reflect.DeepEqual(got, plain) {
		t.Fatalf("no secret columns = %v", got)
	}
	h := RedactHeaders(map[string][]string{"Authorization": {"Bearer x"}, "X-CSRF-TOKEN": {"t"}, "Accept": {"*/*"}})
	if h["Authorization"][0] != Redacted || h["X-CSRF-TOKEN"][0] != Redacted || h["Accept"][0] != "*/*" {
		t.Fatalf("headers = %v", h)
	}
	q := RedactQuery(url.Values{"password": {"p"}, "page": {"2"}})
	if q["password"][0] != Redacted || q["page"][0] != "2" {
		t.Fatalf("query = %v", q)
	}
}

func TestFeedMergesSourcesAndAnalyses(t *testing.T) {
	home := t.TempDir()
	p := pmdir.Paths{Home: home}
	proj := filepath.Join(t.TempDir(), "blog")
	os.MkdirAll(filepath.Join(proj, "storage", "logs"), 0o755)
	os.MkdirAll(p.LogsDir(), 0o755)
	site := config.Site{Name: "blog", Path: proj, Watch: &config.Watch{Enabled: true, Deep: true, NPlusOne: 3}}
	if err := WriteSiteConfig(p, site); err != nil {
		t.Fatal(err)
	}
	f := NewFeed(p, site, "blog.test")
	if got := f.Backlog(100); len(got) != 0 {
		t.Fatalf("empty backlog = %v", got)
	}
	appendFile(t, filepath.Join(p.LogsDir(), "blog.test.log"),
		`{"ts":100.5,"msg":"handled request","request":{"method":"GET","uri":"/posts"},"duration":0.2,"size":10,"status":200,"mullion_rid":"r1"}`+"\n")
	var ev strings.Builder
	for i := 0; i < 3; i++ {
		b, _ := json.Marshal(map[string]any{"type": "query", "t": 100.1, "rid": "r1", "sql": `select * from "users" where "id" = ?`, "bindings": []any{i}, "ms": 1.5, "file": "routes/web.php", "line": 9})
		ev.Write(append(b, '\n'))
	}
	ev.WriteString(`{"type":"query","t":100.2,"rid":"r1","sql":"update users set password = ? where id = ?","bindings":["secret",1],"ms":1}` + "\n")
	ev.WriteString(`{"type":"request","t":100.4,"rid":"r1","method":"GET","uri":"/posts","status":200,"ms":200}` + "\n")
	appendFile(t, EventsPath(p, "blog"), ev.String())
	appendFile(t, filepath.Join(proj, "storage", "logs", "laravel.log"), "[1970-01-01 00:01:41] local.ERROR: boom\n")

	items := f.Poll()
	f.scanned = f.scanned.Add(-time10s) // force a rescan: the log file is new
	items = append(items, f.Poll()...)
	items = append(items, f.Poll()...)
	items = append(items, f.Poll()...) // quiet polls flush the pending log entry
	types := map[string]int{}
	var analysis Item
	for _, it := range items {
		types[it.Type()]++
		if it.Type() == "analysis" {
			analysis = it
		}
		if it.Type() == "query" && strings.Contains(it["sql"].(string), "password") {
			if it["bindings"].([]any)[0] != Redacted {
				t.Fatalf("binding not redacted: %v", it["bindings"])
			}
		}
	}
	if types["request"] != 2 || types["query"] != 4 || types["analysis"] != 1 || types["log"] != 1 {
		t.Fatalf("types = %v", types)
	}
	issues := analysis["issues"].([]QueryIssue)
	if len(issues) == 0 || issues[0].Kind != "n+1" || issues[0].Count != 3 {
		t.Fatalf("analysis = %v", analysis)
	}
	for i := 1; i < len(items); i++ {
		if items[i].Time() < items[i-1].Time() && items[i].Type() != "log" {
			t.Fatalf("items out of order at %d", i)
		}
	}

	// Clear hides older entries and empties our events file.
	if err := Clear(p, "blog"); err != nil {
		t.Fatal(err)
	}
	f.Cleared()
	if fi, _ := os.Stat(EventsPath(p, "blog")); fi.Size() != 0 {
		t.Fatal("events not truncated")
	}
	f2 := NewFeed(p, site, "blog.test")
	if got := f2.Backlog(100); len(got) != 0 {
		t.Fatalf("after clear, backlog = %v", got)
	}
}

func TestEnsureFilesAndCLIEnv(t *testing.T) {
	p := pmdir.Paths{Home: t.TempDir()}
	if err := EnsureFiles(p); err != nil {
		t.Fatal(err)
	}
	ini, err := os.ReadFile(filepath.Join(IniDir(p), "mullion-watch.ini"))
	if err != nil || !strings.Contains(string(ini), `auto_prepend_file="`+filepath.ToSlash(GatePath(p))+`"`) {
		t.Fatalf("ini = %q (%v)", ini, err)
	}
	if _, err := os.Stat(filepath.Join(p.WatchDir(), "mullion-watch-lib.php")); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	env := CLIEnv(p, "blog", "")
	if env["PHP_INI_SCAN_DIR"] != sep+IniDir(p) || env["MULLION_WATCH"] != p.WatchSiteDir("blog") {
		t.Fatalf("env = %v", env)
	}
	if got := CLIEnv(p, "blog", "/etc/php.d")["PHP_INI_SCAN_DIR"]; got != "/etc/php.d"+sep+IniDir(p) {
		t.Fatalf("existing scan dir = %q", got)
	}
	s := SettingsOf(config.Site{Watch: &config.Watch{Enabled: true, NPlusOne: 1}})
	if s.SlowRequestMs != 500 || s.SlowQueryMs != 100 || s.NPlusOne != 5 {
		t.Fatalf("defaults = %+v", s)
	}
}

func TestEditorArgs(t *testing.T) {
	if got := EditorArgs("vscode", "code", "/p/a.php", 12); !reflect.DeepEqual(got, []string{"code", "-g", "/p/a.php:12"}) {
		t.Fatal(got)
	}
	if got := EditorArgs("phpstorm", "phpstorm", "/p/a.php", 0); !reflect.DeepEqual(got, []string{"phpstorm", "--line", "1", "/p/a.php"}) {
		t.Fatal(got)
	}
	root := t.TempDir()
	if _, err := OpenInEditor(root, "../../etc/passwd", 1, ""); err == nil || !strings.Contains(err.Error(), "outside the project") {
		t.Fatalf("escape must be refused: %v", err)
	}
}

// A clear marks where Caddy's log and storage/logs end: their old lines
// stay hidden after a reconnect (a new Feed) even when their timestamps
// can't tell — here they are in the future, so only the offsets hide them.
func TestClearOffsetsSurviveReconnect(t *testing.T) {
	home := t.TempDir()
	p := pmdir.Paths{Home: home}
	proj := filepath.Join(t.TempDir(), "blog")
	logs := filepath.Join(proj, "storage", "logs")
	os.MkdirAll(logs, 0o755)
	os.MkdirAll(p.LogsDir(), 0o755)
	site := config.Site{Name: "blog", Path: proj, Watch: &config.Watch{Enabled: true}}
	caddyLog := filepath.Join(p.LogsDir(), "blog.test.log")
	laravelLog := filepath.Join(logs, "laravel.log")
	future := time.Now().Add(48 * time.Hour)
	req := func(uri string, ts float64) string {
		return fmt.Sprintf(`{"ts":%f,"msg":"handled request","request":{"method":"GET","uri":%q},"duration":0.01,"size":1,"status":200}`+"\n", ts, uri)
	}
	logLine := func(msg string) string {
		return "[" + future.UTC().Format("2006-01-02 15:04:05") + "] local.ERROR: " + msg + "\n"
	}
	os.MkdirAll(p.WatchSiteDir("blog"), 0o755)
	appendFile(t, caddyLog, req("/old", float64(future.Unix())))
	appendFile(t, laravelLog, logLine("old boom"))
	appendFile(t, EventsPath(p, "blog"), `{"type":"dump","t":1,"text":"old"}`+"\n")

	if got := NewFeed(p, site, "blog.test").Backlog(100); len(got) != 3 {
		t.Fatalf("before clear, backlog = %v", got)
	}
	if err := Clear(p, "blog", Sources(p, site, "blog.test")...); err != nil {
		t.Fatal(err)
	}
	f := NewFeed(p, site, "blog.test")
	if got := f.Backlog(100); len(got) != 0 {
		t.Fatalf("after clear, backlog = %v", got)
	}

	// New lines come through live and on the next reconnect; old ones don't.
	appendFile(t, caddyLog, req("/new", float64(future.Unix()+1)))
	appendFile(t, laravelLog, logLine("new boom"))
	// Written after the clear but stamped long before it (a wrong
	// timezone guess): still shown — log lines go by offset, not time.
	appendFile(t, laravelLog, "[2001-01-01 00:00:00] local.INFO: skewed clock\n")
	var live []Item
	for i := 0; i < 4; i++ {
		live = append(live, f.Poll()...)
	}
	texts := func(items []Item) string {
		var b []string
		for _, it := range items {
			b = append(b, fmt.Sprint(it["uri"], it["message"]))
		}
		return strings.Join(b, " | ")
	}
	if s := texts(live); len(live) != 3 || !strings.Contains(s, "/new") || !strings.Contains(s, "new boom") || !strings.Contains(s, "skewed clock") {
		t.Fatalf("live after clear = %s", s)
	}
	again := NewFeed(p, site, "blog.test").Backlog(100)
	if s := texts(again); len(again) != 3 || strings.Contains(s, "/old") || strings.Contains(s, "old boom") {
		t.Fatalf("reconnect after clear = %s", s)
	}

	// Emptying the log file: what is written after it is read whole (the
	// file is now shorter than its mark).
	if files, err := ClearProjectLogs(proj); err != nil || len(files) != 1 {
		t.Fatalf("ClearProjectLogs = %v, %v", files, err)
	}
	appendFile(t, laravelLog, logLine("x"))
	got := NewFeed(p, site, "blog.test").Backlog(100)
	if s := texts(got); !strings.Contains(s, "x") || strings.Contains(s, "boom") {
		t.Fatalf("after emptying laravel.log = %s", s)
	}
	if DataSize(p, "blog") == 0 {
		t.Fatal("DataSize should count clear.json")
	}
}

func TestClearTypesHidesOneView(t *testing.T) {
	p := pmdir.Paths{Home: t.TempDir()}
	site := config.Site{Name: "blog", Path: t.TempDir(), Watch: &config.Watch{Enabled: true, Deep: true}}
	now := float64(time.Now().UnixMilli()) / 1000
	os.MkdirAll(p.WatchSiteDir("blog"), 0o755)
	appendFile(t, EventsPath(p, "blog"), fmt.Sprintf(`{"type":"query","t":%f,"sql":"select 1"}`+"\n"+`{"type":"dump","t":%f,"text":"d"}`+"\n", now-5, now-5))
	if err := ClearTypes(p, "blog", []string{"query"}); err != nil {
		t.Fatal(err)
	}
	f := NewFeed(p, site, "blog.test")
	got := f.Backlog(100)
	if len(got) != 1 || got[0].Type() != "dump" {
		t.Fatalf("after clearing queries = %v", got)
	}
	appendFile(t, EventsPath(p, "blog"), fmt.Sprintf(`{"type":"query","t":%f,"sql":"select 2"}`+"\n", now+5))
	if live := f.Poll(); len(live) != 1 || live[0].Type() != "query" {
		t.Fatalf("new query after clear = %v", live)
	}
	c := LoadClear(p, "blog")
	if c.Types["query"] == 0 || c.At != 0 || c.Stamp() != c.Types["query"] {
		t.Fatalf("clear state = %+v", c)
	}
}
