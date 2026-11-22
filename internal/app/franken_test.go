package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/frankenphp"
	"pm/internal/phpver"
	"pm/internal/pmdir"
)

// frankenTestApp is a sandbox App with one fake installed FrankenPHP
// tag, a global PHP for the php-fpm sites, and the process hooks
// replaced so no test ever downloads or spawns a server.
func frankenTestApp(t *testing.T) (*App, *[]string) {
	t.Helper()
	a := redisTestApp(t)
	a.State.Config.GlobalPHP = "8.3.26"
	bin := frankenphp.Binary(a.Paths, "v1.14.0")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	stopped := &[]string{}
	oldEnsure, oldStop, oldLatest, oldInstall := ensureFranken, stopFranken, latestFranken, installFranken
	t.Cleanup(func() {
		ensureFranken, stopFranken, latestFranken, installFranken = oldEnsure, oldStop, oldLatest, oldInstall
	})
	ensureFranken = func(r frankenphp.Run) (int, error) { return r.Site.Port, nil }
	stopFranken = func(_ pmdir.Paths, site string) { *stopped = append(*stopped, site) }
	latestFranken = func(context.Context) (frankenphp.Release, error) {
		t.Fatal("unexpected FrankenPHP release lookup")
		return frankenphp.Release{}, nil
	}
	installFranken = func(context.Context, pmdir.Paths, frankenphp.Release, func(string)) error {
		t.Fatal("unexpected FrankenPHP install")
		return nil
	}
	return a, stopped
}

func frankenCaddyfile(t *testing.T, a *App) string {
	t.Helper()
	if err := a.WriteCaddyfile(nil, a.EnsureFrankenServers()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.Paths.Caddyfile())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSetSiteServerFranken(t *testing.T) {
	a, _ := frankenTestApp(t)
	addLaravelSite(t, a, "shop", "")
	if err := a.SetSiteServer(context.Background(), "shop", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	s := a.State.FindSite("shop")
	if s.Server != "frankenphp" || !s.OnFranken() || s.Franken == nil {
		t.Fatalf("site = %+v", s)
	}
	if s.Franken.Port < 44001 || s.Franken.Tag != "v1.14.0" || s.Franken.Worker {
		t.Fatalf("franken = %+v", s.Franken)
	}
	out := frankenCaddyfile(t, a)
	if !strings.Contains(out, "reverse_proxy 127.0.0.1:"+strconv.Itoa(s.Franken.Port)) {
		t.Errorf("no reverse_proxy to the FrankenPHP port:\n%s", out)
	}
	if strings.Contains(out, "php_fastcgi") {
		t.Errorf("php_fastcgi still in the Caddyfile:\n%s", out)
	}
	// Persisted.
	st, err := config.Load(a.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.FindSite("shop"); got == nil || !got.OnFranken() || got.Franken.Port != s.Franken.Port {
		t.Errorf("saved site = %+v", got)
	}
}

func TestSwitchBackToFpm(t *testing.T) {
	a, stopped := frankenTestApp(t)
	addLaravelSite(t, a, "shop", "")
	ctx := context.Background()
	if err := a.SetSiteServer(ctx, "shop", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSiteServer(ctx, "shop", "fpm", false, nil); err != nil {
		t.Fatal(err)
	}
	s := a.State.FindSite("shop")
	if s.Server != "" || s.Franken != nil || s.OnFranken() {
		t.Fatalf("site = %+v", s)
	}
	if len(*stopped) == 0 || (*stopped)[0] != "shop" {
		t.Errorf("stopped = %v", *stopped)
	}
	if _, err := phpver.FcgiPort("8.3.26"); err != nil {
		t.Skip("no fcgi port for the test PHP:", err)
	}
	if out := frankenCaddyfile(t, a); !strings.Contains(out, "php_fastcgi") {
		t.Errorf("no php_fastcgi after switching back:\n%s", out)
	}
}

func TestWorkerModeNeedsOctane(t *testing.T) {
	a, _ := frankenTestApp(t)
	dir := addLaravelSite(t, a, "shop", "")
	ctx := context.Background()
	err := a.SetSiteServer(ctx, "shop", "frankenphp", true, nil)
	if err == nil || err.Error() != "worker mode needs Laravel Octane — run: composer require laravel/octane" {
		t.Fatalf("err = %v", err)
	}
	if a.State.FindSite("shop").OnFranken() {
		t.Fatal("refused switch still changed the site")
	}
	lock := `{"packages":[{"name":"laravel/framework"},{"name":"laravel/octane"}],"packages-dev":[]}`
	if err := os.WriteFile(filepath.Join(dir, "composer.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSiteServer(ctx, "shop", "frankenphp", true, nil); err != nil {
		t.Fatal(err)
	}
	if s := a.State.FindSite("shop"); !s.OnFranken() || !s.Franken.Worker {
		t.Errorf("site = %+v", s.Franken)
	}
}

func TestOnlyPHPSites(t *testing.T) {
	a, _ := frankenTestApp(t)
	a.State.AddSite(config.Site{Name: "web", Path: t.TempDir(), Kind: "node", DevPort: 5173})
	err := a.SetSiteServer(context.Background(), "web", "frankenphp", false, nil)
	if err == nil || err.Error() != "only PHP sites can run on FrankenPHP" {
		t.Fatalf("err = %v", err)
	}
}

func TestFrankenPortsAvoidCollisions(t *testing.T) {
	a, _ := frankenTestApp(t)
	a.State.AddSite(config.Site{Name: "web", Path: t.TempDir(), Kind: "node", DevPort: 44001})
	a.State.AddSite(config.Site{Name: "old", Path: t.TempDir(), Server: "frankenphp",
		Franken: &config.Franken{Tag: "v1.14.0", Port: 44002}})
	addLaravelSite(t, a, "shop", "")
	if err := a.SetSiteServer(context.Background(), "shop", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	if p := a.State.FindSite("shop").Franken.Port; p == 44001 || p == 44002 || p < 44001 {
		t.Errorf("port = %d", p)
	}
}

func TestFrankenSiteConfMapping(t *testing.T) {
	a, _ := frankenTestApp(t)
	dir := addLaravelSite(t, a, "shop", "")
	s := a.State.FindSite("shop")
	s.Server, s.Franken = "frankenphp", &config.Franken{Tag: "v1.14.0", Port: 44005}
	s.Secure = true
	s.Perf = &config.Perf{Prod: true}
	var got frankenphp.Run
	ensureFranken = func(r frankenphp.Run) (int, error) { got = r; return r.Site.Port, nil }
	ports := a.EnsureFrankenServers()
	if ports["shop"] != 44005 {
		t.Fatalf("ports = %v", ports)
	}
	c := got.Site
	if c.Name != "shop" || c.Port != 44005 || !c.Secure || !c.FreezeOpcache || c.Env["APP_ENV"] != "production" {
		t.Errorf("conf = %+v", c)
	}
	if c.Root != caddy.DocRoot(dir) || got.ProjectDir != dir || got.Bin != frankenphp.Binary(a.Paths, "v1.14.0") {
		t.Errorf("run = %+v", got)
	}
	// Production-like mode stays out of the main Caddyfile for it.
	out := frankenCaddyfile(t, a)
	if strings.Contains(out, "APP_ENV") {
		t.Errorf("production env leaked into the main Caddyfile:\n%s", out)
	}
}

func TestEnsureFrankenStopsOrphans(t *testing.T) {
	a, stopped := frankenTestApp(t)
	if err := os.WriteFile(filepath.Join(a.Paths.PidsDir(), "franken-gone.pid"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.EnsureFrankenServers()
	if len(*stopped) != 1 || (*stopped)[0] != "gone" {
		t.Errorf("stopped = %v", *stopped)
	}
}

func TestFrankenServerDownIs503(t *testing.T) {
	a, _ := frankenTestApp(t)
	addLaravelSite(t, a, "shop", "")
	if err := a.SetSiteServer(context.Background(), "shop", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	ensureFranken = func(frankenphp.Run) (int, error) { return 0, os.ErrDeadlineExceeded }
	out := frankenCaddyfile(t, a)
	if strings.Contains(out, "reverse_proxy 127.0.0.1:44") || !strings.Contains(out, "503") {
		t.Errorf("a failed server should render the 503:\n%s", out)
	}
}

func TestUpdateFrankenMovesSites(t *testing.T) {
	a, stopped := frankenTestApp(t)
	addLaravelSite(t, a, "shop", "")
	if err := a.SetSiteServer(context.Background(), "shop", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	latestFranken = func(context.Context) (frankenphp.Release, error) {
		return frankenphp.Release{Tag: "v1.15.0"}, nil
	}
	installed := false
	installFranken = func(_ context.Context, p pmdir.Paths, rel frankenphp.Release, _ func(string)) error {
		installed = true
		bin := frankenphp.Binary(p, rel.Tag)
		os.MkdirAll(filepath.Dir(bin), 0o755)
		return os.WriteFile(bin, []byte("fake"), 0o755)
	}
	tag, err := a.UpdateFranken(context.Background(), nil)
	if err != nil || tag != "v1.15.0" || !installed {
		t.Fatalf("tag=%q err=%v installed=%v", tag, err, installed)
	}
	if s := a.State.FindSite("shop"); s.Franken.Tag != "v1.15.0" {
		t.Errorf("tag = %q", s.Franken.Tag)
	}
	if len(*stopped) != 1 || (*stopped)[0] != "shop" {
		t.Errorf("stopped = %v", *stopped)
	}
}

func TestFrankenInfo(t *testing.T) {
	a, _ := frankenTestApp(t)
	addLaravelSite(t, a, "shop", "")
	info, err := a.FrankenInfo("shop")
	if err != nil || info.Server != "fpm" || len(info.Installed) != 1 {
		t.Fatalf("info = %+v %v", info, err)
	}
	if err := a.SetSiteServer(context.Background(), "shop", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	// The fake binary can't report a PHP version; that is left empty.
	info, err = a.FrankenInfo("shop")
	if err != nil || info.Server != "frankenphp" || info.Tag != "v1.14.0" || info.Port < 44001 || info.Running ||
		!strings.HasSuffix(info.Log, "franken-shop.log") {
		t.Fatalf("info = %+v %v", info, err)
	}
}
