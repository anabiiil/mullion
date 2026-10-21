package redis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func pmdirPaths(home string) pmdir.Paths { return pmdir.Paths{Home: home} }

func TestSlug(t *testing.T) {
	cases := []struct{ in, sep, want string }{
		{"Laravel", "_", "laravel"},
		{"My Shop App", "_", "my_shop_app"},
		{"My Shop App", "-", "my-shop-app"},
		{"  Acme  --  CRM!! ", "_", "acme_crm"},
		{"foo_bar-baz", "-", "foo-bar-baz"},
		{"foo_bar-baz", "_", "foo_bar_baz"},
		{"a@b", "-", "a-at-b"},
		{"Café 2", "_", "café_2"},
	}
	for _, c := range cases {
		if got := Slug(c.in, c.sep); got != c.want {
			t.Errorf("Slug(%q, %q) = %q, want %q", c.in, c.sep, got, c.want)
		}
	}
}

func TestParseEnv(t *testing.T) {
	vals := ParseEnv("# c\nAPP_NAME=\"My App\"\nexport A=1\nB='x # y'\nC=bare # comment\nD=\"${APP_NAME} two\"\nE=\nbad line\n")
	want := map[string]string{"APP_NAME": "My App", "A": "1", "B": "x # y", "C": "bare", "D": "My App two", "E": ""}
	for k, v := range want {
		if vals[k] != v {
			t.Errorf("%s = %q, want %q", k, vals[k], v)
		}
	}
	if len(vals) != len(want) {
		t.Errorf("parsed %v", vals)
	}
}

func writeProject(t *testing.T, env string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "artisan"), []byte("<?php"), 0o644)
	if env != "" {
		os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644)
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	return dir
}

func TestProjectSettingsDefaults(t *testing.T) {
	// No config published (Laravel 11+ slim skeleton) and no vendor:
	// the framework default, underscores.
	dir := writeProject(t, "APP_NAME=\"Acme Shop\"\nCACHE_STORE=redis\nREDIS_PORT=6380\n", nil)
	p := ProjectSettings(dir, 6380)
	if p.Prefix != "acme_shop_database_" || p.CachePrefix != "acme_shop_cache_" {
		t.Errorf("prefixes = %q %q", p.Prefix, p.CachePrefix)
	}
	if !p.Laravel || !p.HasEnv || p.Port != 6380 || p.DB != 0 || p.CacheDB != 1 || !p.UsesRedis || !p.PointsHere || p.LegacyCacheKey {
		t.Errorf("settings = %+v", p)
	}
}

func TestProjectSettingsFromConfig(t *testing.T) {
	// Laravel 12 skeleton's published config: dashes.
	db12 := `'options' => [
            'prefix' => env('REDIS_PREFIX', Str::slug((string) env('APP_NAME', 'laravel')).'-database-'),
        ],`
	cache12 := `    'prefix' => env('CACHE_PREFIX', Str::slug((string) env('APP_NAME', 'laravel')).'-cache-'),`
	dir := writeProject(t, "APP_NAME=Acme Shop\nREDIS_DB=3\nREDIS_CACHE_DB=4\nCACHE_DRIVER=redis\n",
		map[string]string{"config/database.php": db12, "config/cache.php": cache12})
	p := ProjectSettings(dir, 6379)
	if p.Prefix != "acme-shop-database-" || p.CachePrefix != "acme-shop-cache-" || p.DB != 3 || p.CacheDB != 4 || !p.LegacyCacheKey {
		t.Errorf("settings = %+v", p)
	}

	// Laravel 10/11 config (underscores) from vendor/, and an explicit
	// REDIS_PREFIX that wins over any default — even an empty one.
	db11 := `'prefix' => env('REDIS_PREFIX', Str::slug(env('APP_NAME', 'laravel'), '_').'_database_'),`
	dir = writeProject(t, "APP_NAME=Shop\nREDIS_PREFIX=\nCACHE_PREFIX=c:\n",
		map[string]string{"vendor/laravel/framework/config/database.php": db11})
	p = ProjectSettings(dir, 6379)
	if p.Prefix != "" || p.CachePrefix != "c:" {
		t.Errorf("explicit prefixes = %q %q", p.Prefix, p.CachePrefix)
	}

	// A literal default in config, and REDIS_PREFIX=null.
	dir = writeProject(t, "REDIS_PREFIX=null\n", map[string]string{"config/cache.php": `'prefix' => env('CACHE_PREFIX', 'myapp:'),`})
	p = ProjectSettings(dir, 6379)
	if p.Prefix != "" || p.CachePrefix != "myapp:" {
		t.Errorf("literal/null prefixes = %q %q", p.Prefix, p.CachePrefix)
	}

	// No .env, no APP_NAME: "laravel".
	p = ProjectSettings(writeProject(t, "", nil), 6379)
	if p.HasEnv || p.Prefix != "laravel_database_" || p.UsesRedis {
		t.Errorf("no .env = %+v", p)
	}
}

func TestEnvPlanApplyDiff(t *testing.T) {
	env := "APP_NAME=Shop\nCACHE_STORE=database\nSESSION_DRIVER=redis\nREDIS_HOST=127.0.0.1\nexport REDIS_PORT=6379\n"
	want := LaravelEnv(6380, "", false)
	plan := PlanEnv(env, want)
	keys := []string{}
	for _, c := range plan {
		keys = append(keys, c.Key)
	}
	// REDIS_HOST and SESSION_DRIVER are already right.
	if strings.Join(keys, ",") != "REDIS_CLIENT,REDIS_PASSWORD,REDIS_PORT,CACHE_STORE,QUEUE_CONNECTION" {
		t.Fatalf("plan keys = %v", keys)
	}
	out := ApplyEnv(env, plan)
	vals := ParseEnv(out)
	for _, kv := range want {
		if vals[kv[0]] != kv[1] {
			t.Errorf("after apply %s = %q, want %q", kv[0], vals[kv[0]], kv[1])
		}
	}
	if !strings.Contains(out, "export REDIS_PORT=6380\n") || !strings.Contains(out, "CACHE_STORE=redis\n") {
		t.Errorf("in-place edits lost:\n%s", out)
	}
	if !strings.Contains(out, "\n\n# Redis (added by Mullion)\nREDIS_CLIENT=phpredis\nREDIS_PASSWORD=null\nQUEUE_CONNECTION=redis\n") {
		t.Errorf("appended block wrong:\n%s", out)
	}
	if len(PlanEnv(out, want)) != 0 {
		t.Error("applying the plan should leave nothing to change")
	}
	diff := DiffEnv(plan)
	if !strings.Contains(diff, "- CACHE_STORE=database\n+ CACHE_STORE=redis\n") || !strings.Contains(diff, "+ REDIS_CLIENT=phpredis\n") ||
		strings.Contains(diff, "- REDIS_CLIENT") {
		t.Errorf("diff:\n%s", diff)
	}
	// Laravel ≤ 10 uses CACHE_DRIVER; a password is written quoted when needed.
	legacy := LaravelEnv(6379, "p w", true)
	if legacy[4][0] != "CACHE_DRIVER" || legacy[2][1] != "p w" {
		t.Errorf("legacy = %v", legacy)
	}
	if out := ApplyEnv("", PlanEnv("", legacy)); !strings.Contains(out, `REDIS_PASSWORD="p w"`) || strings.HasPrefix(out, "\n") {
		t.Errorf("empty env apply:\n%q", out)
	}
}
