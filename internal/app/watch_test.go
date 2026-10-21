package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/watch"
)

func TestSetWatchCaddyfileAndEnv(t *testing.T) {
	a := sandboxApp(t)
	proj := t.TempDir()
	a.State.Config.GlobalPHP = "8.4.23"
	a.State.AddSite(config.Site{Name: "blog", Path: proj})
	a.State.AddSite(config.Site{Name: "shop", Path: t.TempDir()})

	envOf := func(site string) map[string]string {
		m := map[string]string{}
		for _, kv := range a.SiteEnv(*a.State.FindSite(site)) {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}
	caddyfile := func() string {
		if err := a.WriteCaddyfile(nil); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(a.Paths.Caddyfile())
		return string(b)
	}

	// Off: nothing anywhere.
	if c := caddyfile(); strings.Contains(c, "PHP_VALUE") {
		t.Fatalf("watch off must not touch the Caddyfile:\n%s", c)
	}
	if _, ok := envOf("blog")["MULLION_WATCH"]; ok {
		t.Fatal("watch off must not set MULLION_WATCH")
	}

	// Requests & logs only: still no prepend.
	if err := a.SetWatch("blog", config.Watch{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if c := caddyfile(); strings.Contains(c, "PHP_VALUE") {
		t.Fatalf("level 1 needs no prepend:\n%s", c)
	}

	// Dumps + deep: PHP_VALUE for blog only, files written, CLI env set.
	if err := a.SetWatch("blog", config.Watch{Enabled: true, Dumps: true, Deep: true, NPlusOne: 3}); err != nil {
		t.Fatal(err)
	}
	c := caddyfile()
	if !strings.Contains(c, "env PHP_VALUE") || !strings.Contains(c, "env MULLION_WATCH \""+a.Paths.WatchSiteDir("blog")+"\"") {
		t.Fatalf("watched site block missing:\n%s", c)
	}
	if strings.Count(c, "PHP_VALUE") != 1 {
		t.Fatalf("only the watched site gets the prepend:\n%s", c)
	}
	for _, f := range []string{watch.GatePath(a.Paths), filepath.Join(a.Paths.WatchDir(), "mullion-watch-lib.php"),
		filepath.Join(a.Paths.WatchSiteDir("blog"), "config.json")} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	env := envOf("blog")
	if env["MULLION_WATCH"] != a.Paths.WatchSiteDir("blog") || !strings.HasSuffix(env["PHP_INI_SCAN_DIR"], watch.IniDir(a.Paths)) {
		t.Fatalf("CLI env = %v / %v", env["MULLION_WATCH"], env["PHP_INI_SCAN_DIR"])
	}
	if _, ok := envOf("shop")["MULLION_WATCH"]; ok {
		t.Fatal("an unwatched site's commands must not load the prepend")
	}
	info, err := a.WatchStatus("blog")
	if err != nil || !info.Prepend || info.Settings.NPlusOne != 3 || info.Settings.SlowQueryMs != watch.DefaultSlowQueryMs {
		t.Fatalf("status = %+v, %v", info, err)
	}

	// Validation.
	if err := a.SetWatch("blog", config.Watch{Enabled: true, NPlusOne: 1}); err == nil {
		t.Fatal("N+1 threshold 1 must be refused")
	}
	if err := a.SetWatch("blog", config.Watch{Enabled: true, Editor: "notepad++"}); err == nil {
		t.Fatal("unknown editor must be refused")
	}

	// Off again: gone from the Caddyfile; the prepend files stay (php-fpm
	// may still carry PHP_VALUE for other sites until it restarts).
	if err := a.SetWatch("blog", config.Watch{}); err != nil {
		t.Fatal(err)
	}
	if c := caddyfile(); strings.Contains(c, "PHP_VALUE") {
		t.Fatalf("watch off:\n%s", c)
	}
	if _, err := os.Stat(watch.GatePath(a.Paths)); err != nil {
		t.Fatal("the gate file must survive turning the watcher off")
	}
}
