package ui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pm/internal/app"
	"pm/internal/config"
)

// The server choice answers a bad value at once — before any job (and
// so any FrankenPHP download) starts.
func TestSiteServerRejectsUnknownServer(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("SystemDrive", home)
	} else {
		t.Setenv("HOME", home)
	}
	a, err := app.New()
	if err != nil {
		t.Fatal(err)
	}
	a.State.AddSite(config.Site{Name: "shop", Path: t.TempDir(), Kind: "php"})
	if err := a.State.Save(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(newMux("tok"))
	defer srv.Close()

	ok, msg := postJSON(t, srv, "/api/sites/server", map[string]any{"name": "shop", "server": "bogus"}, nil)
	if ok || !strings.Contains(msg, "server must be fpm or frankenphp") {
		t.Fatalf("bogus server: ok=%v error=%q, want the refusal", ok, msg)
	}
	ok, msg = postJSON(t, srv, "/api/sites/server", map[string]any{"name": "nope", "server": "frankenphp"}, nil)
	if ok || !strings.Contains(msg, `no site named "nope"`) {
		t.Fatalf("unknown site: ok=%v error=%q", ok, msg)
	}

	// A site on php-fpm reports so, with no FrankenPHP installed.
	var info struct {
		Server    string
		Installed []string
	}
	if ok, msg := postJSON(t, srv, "/api/sites/franken", map[string]string{"name": "shop"}, &info); !ok {
		t.Fatalf("/api/sites/franken: %s", msg)
	}
	if info.Server != "fpm" || info.Installed == nil || len(info.Installed) != 0 {
		t.Fatalf("franken info = %+v, want fpm and no installs", info)
	}
}

func TestTailLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	if got := tailLines(p, 3); got != "" {
		t.Fatalf("missing file = %q", got)
	}
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailLines(p, 2); got != "c\nd" {
		t.Fatalf("tail 2 = %q", got)
	}
	if got := tailLines(p, 10); got != "a\nb\nc\nd" {
		t.Fatalf("tail 10 = %q", got)
	}
}
