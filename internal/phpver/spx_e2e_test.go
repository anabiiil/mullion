//go:build darwin

package phpver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// TestEnsureSpxEndToEnd runs the real SPX build: it downloads SPX's
// release tarball and the php.net sources (~14MB) and compiles spx.so,
// so it only runs with MULLION_SPX_E2E=1. MULLION_SPX_E2E_HOME is the
// sandbox mullion home to build into (kept afterwards, for a manual
// php-fpm run); MULLION_SPX_E2E_PHP a directory holding copies of the
// php and php-fpm binaries of e2eSourceVersion. Neither may be the real
// ~/.mullion.
func TestEnsureSpxEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_SPX_E2E") != "1" {
		t.Skip("set MULLION_SPX_E2E=1 (+ MULLION_SPX_E2E_HOME, MULLION_SPX_E2E_PHP) to run: downloads ~14MB and compiles for ~1 min")
	}
	home, src := os.Getenv("MULLION_SPX_E2E_HOME"), os.Getenv("MULLION_SPX_E2E_PHP")
	if home == "" || src == "" {
		t.Fatal("MULLION_SPX_E2E_HOME and MULLION_SPX_E2E_PHP are required")
	}
	if real, err := os.UserHomeDir(); err == nil && strings.HasPrefix(filepath.Clean(home), filepath.Join(real, ".mullion")) {
		t.Fatal("MULLION_SPX_E2E_HOME must not be the real ~/.mullion")
	}
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	verDir := paths.PhpVersionDir(e2eSourceVersion)
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{PhpExeName, PhpFpmName} {
		if err := copyExecutable(filepath.Join(src, bin), filepath.Join(verDir, bin)); err != nil {
			t.Fatalf("copying %s: %v", bin, err)
		}
	}
	if err := writeIni(verDir); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	if err := EnsureSpx(ctx, paths, e2eSourceVersion); err != nil {
		t.Fatalf("EnsureSpx: %v", err)
	}
	t.Logf("EnsureSpx took %s", time.Since(start).Round(time.Second))
	if !SpxLoaded(paths, e2eSourceVersion) {
		t.Fatal("SpxLoaded is false after EnsureSpx")
	}
	if _, err := os.Stat(filepath.Join(SpxAssetsDir(paths), "index.html")); err != nil {
		t.Fatalf("SPX web UI not installed: %v", err)
	}
	php := filepath.Join(verDir, PhpExeName)
	out, err := exec.Command(php, iniArgs(php, "-m")...).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "SPX") {
		t.Fatalf("php -m does not list SPX: %v\n%s", err, out)
	}
	if err := VerifyInstall(paths, e2eSourceVersion); err != nil {
		t.Fatalf("PHP does not start cleanly with spx.so: %v", err)
	}

	// Idempotent: a second call is quick and changes nothing.
	ini, _ := os.ReadFile(filepath.Join(verDir, "php.ini"))
	if err := EnsureSpx(ctx, paths, e2eSourceVersion); err != nil {
		t.Fatalf("second EnsureSpx: %v", err)
	}
	ini2, _ := os.ReadFile(filepath.Join(verDir, "php.ini"))
	if string(ini) != string(ini2) {
		t.Fatalf("second EnsureSpx changed php.ini:\n%s", ini2)
	}

	// A CLI profile lands in spx.data_dir as a full report.
	data := t.TempDir()
	script := filepath.Join(data, "t.php")
	os.WriteFile(script, []byte(`<?php function f($n){return $n<2?$n:f($n-1)+f($n-2);} echo f(15);`), 0o644)
	cmd := exec.Command(php, iniArgs(php, "-d", "spx.data_dir="+data, script)...)
	cmd.Env = append(os.Environ(), "SPX_ENABLED=1", "SPX_REPORT=full")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("profiled run: %v\n%s", err, out)
	}
	if m, _ := filepath.Glob(filepath.Join(data, "spx-full-*.txt.gz")); len(m) != 1 {
		t.Fatalf("expected one report in %s, got %v", data, m)
	}
}
