package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"pm/internal/config"
)

func TestProdModeCaddyfile(t *testing.T) {
	a := sandboxApp(t)
	a.State.Config.GlobalPHP = "8.4.23"
	a.State.Config.TLD = "test"
	a.State.AddSite(config.Site{Name: "blog", Path: t.TempDir()})
	a.State.AddSite(config.Site{Name: "shop", Path: t.TempDir()})
	caddyfile := func() string {
		if err := a.WriteCaddyfile(nil); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(a.Paths.Caddyfile())
		return string(b)
	}
	if c := caddyfile(); strings.Contains(c, "validate_timestamps") || strings.Contains(c, "APP_ENV") {
		t.Fatalf("off:\n%s", c)
	}
	if err := a.SetProdMode(context.Background(), "blog", true, false, nil); err != nil {
		t.Fatal(err)
	}
	c := caddyfile()
	blog := c[strings.Index(c, "blog.test"):strings.Index(c, "shop.test")]
	shop := c[strings.Index(c, "shop.test"):]
	if !strings.Contains(blog, `env APP_ENV "production"`) || !strings.Contains(blog, `env APP_DEBUG "false"`) {
		t.Fatalf("prod env:\n%s", blog)
	}
	if prodFreezesOpcache {
		if !strings.Contains(blog, "opcache.validate_timestamps=0") || !strings.Contains(shop, "opcache.validate_timestamps=1") {
			t.Fatalf("timestamps:\n%s", c)
		}
	}
	if strings.Contains(shop, "APP_ENV") {
		t.Fatal("only the prod site gets the env")
	}
	if err := a.SetProdMode(context.Background(), "blog", false, false, nil); err != nil {
		t.Fatal(err)
	}
	if c := caddyfile(); strings.Contains(c, "validate_timestamps") || strings.Contains(c, "APP_ENV") {
		t.Fatalf("off again:\n%s", c)
	}
}

func TestLoadConfigTargets(t *testing.T) {
	a := sandboxApp(t)
	a.State.Config.TLD = "test"
	a.State.AddSite(config.Site{Name: "blog", Path: t.TempDir(), Aliases: []string{"api"}, DevPort: 5173})
	ok := []string{"/", "/posts?x=1", "http://blog.test/a", "http://api.blog.test/", "http://localhost:5173/"}
	for _, u := range ok {
		c, err := a.LoadConfig("blog", LoadInput{URL: u, Concurrency: 5, DurationS: 10})
		if err != nil {
			t.Errorf("%s: %v", u, err)
		} else if c.Activity == nil || c.Headers["User-Agent"] == "" {
			t.Errorf("%s: config %+v", u, c)
		}
	}
	bad := []string{"http://example.com/", "http://shop.test/", "http://localhost:3306/", "http://127.0.0.1/", "ftp://blog.test/", "http://blog.test.evil.com/"}
	for _, u := range bad {
		if _, err := a.LoadConfig("blog", LoadInput{URL: u, Concurrency: 5, DurationS: 10}); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := a.LoadConfig("blog", LoadInput{URL: "/", Concurrency: 500, DurationS: 10}); err == nil {
		t.Error("concurrency 500 accepted")
	}
	if _, err := a.LoadConfig("blog", LoadInput{URL: "/", Concurrency: 5, DurationS: 10, Headers: map[string]string{"X\r\nY": "1"}}); err == nil {
		t.Error("bad header accepted")
	}
}

func TestPerfIgnoreAndBudgets(t *testing.T) {
	a := sandboxApp(t)
	a.State.AddSite(config.Site{Name: "blog", Path: t.TempDir()})
	if err := a.SetPerfIgnored("blog", "php.eol", true); err != nil {
		t.Fatal(err)
	}
	a.SetPerfIgnored("blog", "php.eol", true)
	if s := a.State.FindSite("blog"); len(s.Perf.Ignored) != 1 {
		t.Fatal(s.Perf.Ignored)
	}
	a.SetPerfIgnored("blog", "php.eol", false)
	if s := a.State.FindSite("blog"); len(s.Perf.Ignored) != 0 {
		t.Fatal(s.Perf.Ignored)
	}
	a.SetPerfBudgets("blog", PerfBudgets(config.Site{}))
	if b := PerfBudgets(*a.State.FindSite("blog")); b.MaxMs != 300 || b.MaxQueries != 30 || b.MaxMemoryMB != 64 {
		t.Fatal(b)
	}
}

func TestProdEnvSkipsAppEnvWithEnvFile(t *testing.T) {
	dir := t.TempDir()
	s := config.Site{Name: "x", Path: dir}
	if e := prodEnv(s); e["APP_ENV"] != "production" || e["APP_DEBUG"] != "false" {
		t.Fatal(e)
	}
	os.WriteFile(dir+"/.env.production", []byte("DB_HOST=prod\n"), 0o644)
	if e := prodEnv(s); e["APP_ENV"] != "" || e["APP_DEBUG"] != "false" {
		t.Fatalf("with .env.production: %v", e)
	}
}
