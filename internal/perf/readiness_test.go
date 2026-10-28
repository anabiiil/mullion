package perf

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pm/internal/watch"
	"pm/internal/watch/explain"
)

func byID(fs []Finding) map[string]Finding {
	m := map[string]Finding{}
	for _, f := range fs {
		m[f.ID] = f
	}
	return m
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckLaravel(t *testing.T) {
	routes, err := ParseRouteList(readFixture(t, "routes.json"))
	if err != nil || len(routes) != 4 {
		t.Fatalf("routes: %v %v", routes, err)
	}
	f := Facts{
		Laravel: true, LaravelVersion: 11,
		Env:         map[string]string{"APP_ENV": "local", "APP_DEBUG": "true", "APP_KEY": "base64:x", "LOG_LEVEL": "debug", "QUEUE_CONNECTION": "sync", "SESSION_DRIVER": "file"},
		ProdEnv:     map[string]string{"APP_ENV": "production", "APP_DEBUG": "true", "CACHE_STORE": "array"},
		ProdEnvFile: ".env.production",
		Composer:    readFixture(t, "composer.json"),
		Routes:      routes,
		AppProvider: "<?php class AppServiceProvider { public function boot() {} }",
		EnvCalls:    FindEnvCalls(map[string]string{"app/Http/Controllers/X.php": "<?php\n// env('NOPE')\n$k = env('STRIPE_KEY');\n$x = $this->env('a');\n", "app/Y.php": "<?php echo Env::get('x');"}),
		PHPVersion:  "8.2.20",
		Opcache:     &OpcacheFacts{Enabled: true, Memory: 64, MaxFiles: 10000, ValidateTimestamps: true},
		PHPFiles:    14000,
		Now:         time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
	}
	got := byID(CheckLaravel(f))
	want := map[string]string{
		"laravel.app-debug":                                 SevHigh, // .env.production says true
		"laravel.log-level":                                 SevInfo, // only the local .env says it
		"laravel.queue-sync":                                SevInfo,
		"laravel.session-driver":                            SevInfo,
		"laravel.cache-store":                               SevHigh, // array in production
		"laravel.route-names":                               SevHigh,
		"laravel.route-closures":                            SevLow,
		"laravel.env-outside-config":                        SevMedium,
		"laravel.strict-models":                             SevLow,
		"laravel.optimize":                                  SevInfo,
		"composer.dev-in-require:barryvdh/laravel-debugbar": SevMedium,
		"composer.telescope":                                SevMedium,
		"composer.optimize-autoloader":                      SevLow,
		"php.eol-soon":                                      SevMedium,
		"php.opcache-max-files":                             SevMedium,
		"php.opcache-memory":                                SevLow,
		"php.opcache-prod":                                  SevInfo,
	}
	for id, sev := range want {
		if g, ok := got[id]; !ok || g.Severity != sev {
			t.Errorf("%s: got %+v, want severity %s", id, g, sev)
		}
	}
	for _, no := range []string{"laravel.app-env", "laravel.app-key", "php.eol", "php.opcache-off"} {
		if _, ok := got[no]; ok {
			t.Errorf("unexpected %s", no)
		}
	}
	if w := got["laravel.env-outside-config"].Where; len(w) != 1 || !strings.Contains(w[0], "X.php:3") {
		t.Errorf("env() calls: %v", w)
	}
	if !strings.Contains(got["php.opcache-max-files"].Fix, "16229") {
		t.Errorf("max files fix: %s", got["php.opcache-max-files"].Fix)
	}
	if w := got["laravel.route-names"].Where; len(w) != 1 || !strings.HasPrefix(w[0], "posts.index:") {
		t.Errorf("dup names: %v", w)
	}
	// Laravel 8: closures break route:cache.
	f.LaravelVersion = 8
	if byID(CheckLaravel(f))["laravel.route-closures"].Severity != SevHigh {
		t.Error("closures on Laravel 8 must be high")
	}
	// A broken app: one boot finding instead of the route rules.
	f.Routes, f.RoutesErr = nil, "exit status 1: Class \"Foo\" not found"
	g2 := byID(CheckLaravel(f))
	if _, ok := g2["laravel.boot"]; !ok {
		t.Error("boot failure")
	}
	if _, ok := g2["laravel.optimize"]; ok {
		t.Error("optimize suggested for an app that doesn't boot")
	}
	// EOL PHP, empty key.
	f.PHPVersion, f.Env["APP_KEY"] = "8.1.30", ""
	g3 := byID(CheckLaravel(f))
	if g3["php.eol"].Severity != SevHigh || g3["laravel.app-key"].Severity != SevHigh {
		t.Errorf("eol/key: %+v %+v", g3["php.eol"], g3["laravel.app-key"])
	}
}

func TestCheckSchema(t *testing.T) {
	var s explain.Schema
	if err := json.Unmarshal(readFixture(t, "schema.json"), &s); err != nil {
		t.Fatal(err)
	}
	got := byID(CheckSchema(&s))
	if f, ok := got["db.fk-index:posts.user_id"]; !ok || f.Severity != SevMedium || !strings.Contains(f.Fix, `CREATE INDEX "posts_user_id_index" ON "posts" ("user_id");`) || !strings.Contains(f.Fix, "$table->index('user_id');") {
		t.Errorf("fk without index: %+v", f)
	}
	if _, ok := got["db.fk-index:comments.post_id"]; ok {
		t.Error("comments.post_id is the leading column of an index")
	}
	if _, ok := got["db.no-pk:logs"]; !ok {
		t.Error("logs has no primary key")
	}
	if _, ok := got["db.no-pk:sessions"]; ok {
		t.Error("Laravel's own tables are skipped")
	}
	if _, ok := got["db.big-no-index:posts"]; !ok {
		t.Error("posts: 20k rows, no secondary index")
	}
	if f, ok := got["db.id-index:users"]; !ok || !strings.Contains(f.Title, "team_id") {
		t.Errorf("users.team_id: %+v", got)
	}
	if len(got) != 4 {
		t.Errorf("findings: %v", got)
	}
}

func TestFrontend(t *testing.T) {
	dir := t.TempDir()
	assets := filepath.Join(dir, "public", "build", "assets")
	os.MkdirAll(assets, 0o755)
	os.MkdirAll(filepath.Join(dir, "resources", "images"), 0o755)
	// A big "minified" file: one long line that doesn't compress well.
	var b strings.Builder
	rnd := make([]byte, 600<<10)
	_, _ = rand.Read(rnd)
	b.WriteString("var x=\"" + hex.EncodeToString(rnd) + "\";")
	os.WriteFile(filepath.Join(assets, "app-BkL3x9Qa.js"), []byte(b.String()), 0o644)
	os.WriteFile(filepath.Join(assets, "app-BkL3x9Qa.js.map"), []byte("{}"), 0o644)
	src := strings.Repeat("function hello() {\n  return 1;\n}\n", 400)
	os.WriteFile(filepath.Join(assets, "dev.js"), []byte(src), 0o644)
	os.WriteFile(filepath.Join(assets, "app-Cx.css"), []byte("body{color:red}"), 0o644)
	os.WriteFile(filepath.Join(dir, "resources", "images", "hero.png"), make([]byte, 400<<10), 0o644)

	if d := FindBuildDir(dir); d != filepath.Join("public", "build") {
		t.Fatalf("build dir %q", d)
	}
	r := AnalyzeBuild(dir, FindBuildDir(dir))
	if r.Maps != 1 || r.Files != 4 || r.JSSize < 1000<<10 || r.CSSSize != 15 || len(r.Images) != 1 {
		t.Fatalf("report: %+v", r)
	}
	got := byID(CheckFrontend(r, "npm run build"))
	for _, id := range []string{"frontend.js-size", "frontend.big-chunks", "frontend.unminified", "frontend.source-maps", "frontend.big-images"} {
		if _, ok := got[id]; !ok {
			t.Errorf("missing %s (got %v)", id, got)
		}
	}
	if w := got["frontend.unminified"].Where; len(w) != 1 || !strings.Contains(w[0], "dev.js") {
		t.Errorf("unminified: %v", w)
	}
	none := byID(CheckFrontend(AnalyzeBuild(t.TempDir(), ""), "npm run build"))
	if f := none["frontend.no-build"]; f.Command != "npm run build" {
		t.Errorf("no build: %+v", none)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestSummarizeQueries(t *testing.T) {
	items := []watch.Item{
		{"type": "query", "sql": "select * from users where id = ?", "bindings": []any{1.0}, "ms": 1.0, "conn": "sqlite"},
		{"type": "query", "sql": "select * from posts order by created_at", "bindings": []any{}, "ms": 250.0, "file": "app/X.php", "line": 3.0},
		{"type": "analysis", "issues": []any{map[string]any{"kind": "n+1", "sql": "select * from users where id = ?", "count": 12.0, "ms": 6.0, "file": "app/Y.php", "line": 9.0}}},
		{"type": "analysis", "issues": []any{map[string]any{"kind": "n+1", "sql": "select * from users where id = ?", "count": 8.0, "ms": 4.0, "file": "app/Y.php", "line": 9.0}}},
	}
	qs := SummarizeQueries(items, 100, 5)
	if len(qs) != 2 || qs[0].Kind != "n+1" || qs[0].Requests != 2 || qs[0].Count != 20 || qs[0].MaxMs != 12 || qs[0].Example == "" || qs[0].Conn != "sqlite" {
		t.Fatalf("%+v", qs)
	}
	if qs[1].Kind != "slow" || qs[1].MaxMs != 250 || qs[1].File != "app/X.php" {
		t.Fatalf("%+v", qs[1])
	}
	fs := CheckQueries(qs)
	if len(fs) != 2 || fs[0].Explain == nil || fs[0].Explain.NPlusOne != 12 || !strings.HasPrefix(fs[0].ID, "queries.n+1:") {
		t.Fatalf("%+v", fs)
	}
	// Stable ids across runs.
	if CheckQueries(qs)[0].ID != fs[0].ID {
		t.Fatal("ids must be stable")
	}
}

func TestMarkIgnoredAndSort(t *testing.T) {
	fs := []Finding{{ID: "a", Severity: SevLow}, {ID: "b", Severity: SevHigh}, {ID: "c", Severity: SevHigh}}
	MarkIgnored(fs, []string{"c"})
	SortFindings(fs)
	if fs[0].ID != "b" || fs[1].ID != "a" || fs[2].ID != "c" || !fs[2].Ignored {
		t.Fatalf("%+v", fs)
	}
}
