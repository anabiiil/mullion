//go:build frankenreal

package app

// Opt-in check of the app layer against a real FrankenPHP binary:
//
//	FRANKEN_REAL_BIN=/path/to/frankenphp \
//	  go test -tags frankenreal -run TestFrankenReal -v ./internal/app/
//
// Everything lives in a temp directory (no ~/.mullion, no hosts file) and
// the main Caddy on 80/443 is never started: only EnsureFrankenServers
// and WriteCaddyfile run. No network is needed.

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/frankenphp"
)

const realIndexPHP = `<?php
if (isset($_GET['env'])) {
    echo getenv('APP_ENV') ?: ($_SERVER['APP_ENV'] ?? 'unset');
    return;
}
echo $_SERVER['HTTP_HOST'], '|', $_SERVER['HTTPS'] ?? 'off', '|', PHP_VERSION;
`

func realGet(t *testing.T, port int, query string) string {
	t.Helper()
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "demo.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d: %s", query, resp.StatusCode, body)
	}
	return strings.TrimSpace(string(body))
}

func realPid(t *testing.T, a *App) int {
	t.Helper()
	data, err := os.ReadFile(frankenphp.PidPath(a.Paths, "demo"))
	if err != nil {
		t.Fatalf("no pid file: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestFrankenReal(t *testing.T) {
	src := os.Getenv("FRANKEN_REAL_BIN")
	if src == "" {
		t.Skip("set FRANKEN_REAL_BIN to a real frankenphp binary")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	a := redisTestApp(t)
	a.Progress = func(msg string) { t.Log(msg) }
	t.Cleanup(func() { frankenphp.StopAll(a.Paths) })

	// Seed the install where Binary expects it, as Install would.
	bin := frankenphp.Binary(a.Paths, "v1.14.0")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := frankenphp.PHPVersion(bin); err != nil {
		t.Fatalf("PHPVersion: %v", err)
	} else {
		t.Logf("binary embeds PHP %s", v)
	}

	// A plain PHP site (no artisan): index.php in the project root.
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.php"), []byte(realIndexPHP), 0o644); err != nil {
		t.Fatal(err)
	}
	a.State.AddSite(config.Site{Name: "demo", Path: dir, Secure: true})

	// 1. php-fpm -> FrankenPHP.
	if err := a.SetSiteServer(t.Context(), "demo", "frankenphp", false, nil); err != nil {
		t.Fatal(err)
	}
	site := a.State.FindSite("demo")
	port := site.Franken.Port
	if portOpen(port) {
		t.Fatalf("port %d is already in use on this machine", port)
	}
	ports := a.EnsureFrankenServers()
	if ports["demo"] != port {
		t.Fatalf("EnsureFrankenServers = %v, want demo on %d", ports, port)
	}
	if err := a.WriteCaddyfile(nil, ports); err != nil {
		t.Fatal(err)
	}
	caddyfile, _ := os.ReadFile(a.Paths.Caddyfile())
	if !strings.Contains(string(caddyfile), "reverse_proxy 127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("main Caddyfile does not proxy to the FrankenPHP port:\n%s", caddyfile)
	}

	got := realGet(t, port, "")
	t.Logf("GET / (Host: demo.test, X-Forwarded-Proto: https) -> %s", got)
	if !strings.HasPrefix(got, "demo.test|on|") {
		t.Fatalf("response = %q, want demo.test|on|<php version>", got)
	}
	if env := realGet(t, port, "?env=1"); env == "production" {
		t.Fatalf("APP_ENV is already production before the mode is on")
	}

	// A second Ensure with nothing changed leaves the server alone.
	pid1 := realPid(t, a)
	a.EnsureFrankenServers()
	if pid := realPid(t, a); pid != pid1 {
		t.Errorf("unchanged config restarted the server: pid %d -> %d", pid1, pid)
	}

	// 2. Production-like mode restarts it with APP_ENV=production.
	if err := a.SetProdMode(t.Context(), "demo", true, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.EnsureFrankenServers()["demo"]; got != port {
		t.Fatalf("port after prod mode = %d, want %d", got, port)
	}
	pid2 := realPid(t, a)
	t.Logf("pid %d -> %d after production-like mode", pid1, pid2)
	if pid2 == pid1 {
		t.Errorf("server was not restarted by production-like mode (pid %d)", pid1)
	}
	if env := realGet(t, port, "?env=1"); env != "production" {
		t.Errorf("APP_ENV = %q, want production", env)
	} else {
		t.Logf("GET /?env=1 -> %s", env)
	}
	if got := realGet(t, port, ""); !strings.HasPrefix(got, "demo.test|on|") {
		t.Errorf("response after prod mode = %q", got)
	}

	// 3. Back to php-fpm: the process is gone and the port closed.
	if err := a.SetSiteServer(t.Context(), "demo", "fpm", false, nil); err != nil {
		t.Fatal(err)
	}
	a.EnsureFrankenServers()
	if frankenphp.Running(a.Paths, "demo") {
		t.Errorf("server still running after switching back to php-fpm")
	}
	if processAliveReal(pid2) {
		t.Errorf("pid %d is still alive", pid2)
	}
	deadline := time.Now().Add(5 * time.Second)
	for portOpen(port) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if portOpen(port) {
		t.Errorf("port %d still open after switching back", port)
	}
	t.Logf("after switching back to fpm: running=%v port %d open=%v", frankenphp.Running(a.Paths, "demo"), port, portOpen(port))
}

func processAliveReal(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
