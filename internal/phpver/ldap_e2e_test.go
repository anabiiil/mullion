//go:build darwin

package phpver

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// TestEnsureLdapEndToEnd exercises the real build: it downloads the
// php.net source tarball (~14MB) and compiles ext/ldap against the
// macOS SDK, so it's skipped unless MULLION_LDAP_E2E=1 is set. It never
// touches the real ~/.mullion — it only reads (copies) the php and
// php-fpm binaries already installed there for e2eSourceVersion, into a
// throwaway temp HOME.
const e2eSourceVersion = "8.4.23"

func TestEnsureLdapEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_LDAP_E2E") != "1" {
		t.Skip("set MULLION_LDAP_E2E=1 to run (downloads ~14MB from php.net and compiles for ~1-2 min)")
	}

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolving real home dir: %v", err)
	}
	srcDir := filepath.Join(realHome, ".mullion", "php", e2eSourceVersion)
	srcPhp := filepath.Join(srcDir, PhpExeName)
	srcFpm := filepath.Join(srcDir, PhpFpmName)
	for _, p := range []string{srcPhp, srcFpm} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to already be installed on this machine for the e2e test: %v", p, err)
		}
	}

	tempHome := t.TempDir()
	paths := pmdir.Paths{Home: tempHome}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}

	verDir := paths.PhpVersionDir(e2eSourceVersion)
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", verDir, err)
	}
	copyStart := time.Now()
	if err := copyExecutable(srcPhp, filepath.Join(verDir, PhpExeName)); err != nil {
		t.Fatalf("copying php: %v", err)
	}
	if err := copyExecutable(srcFpm, filepath.Join(verDir, PhpFpmName)); err != nil {
		t.Fatalf("copying php-fpm: %v", err)
	}
	t.Logf("copied php + php-fpm from %s in %s", srcDir, time.Since(copyStart))

	// A minimal php.ini WITHOUT any ldap line — EnsureLdap must add one.
	iniPath := filepath.Join(verDir, "php.ini")
	minimalIni := "display_errors=On\nmemory_limit=512M\n"
	if err := os.WriteFile(iniPath, []byte(minimalIni), 0o644); err != nil {
		t.Fatalf("writing minimal php.ini: %v", err)
	}

	php := filepath.Join(verDir, PhpExeName)
	if has, err := phpHasLdap(php); err != nil {
		t.Fatalf("checking baseline php -m: %v", err)
	} else if has {
		t.Fatalf("copied php binary unexpectedly already has ldap loaded (check %s)", iniPath)
	}

	t.Logf("running EnsureLdap for PHP %s in temp HOME %s ...", e2eSourceVersion, tempHome)
	buildStart := time.Now()
	if err := EnsureLdap(context.Background(), paths, e2eSourceVersion); err != nil {
		t.Fatalf("EnsureLdap: %v", err)
	}
	t.Logf("EnsureLdap completed in %s", time.Since(buildStart))

	soPath := filepath.Join(verDir, "ext", "ldap.so")
	if _, err := os.Stat(soPath); err != nil {
		t.Fatalf("expected %s to exist after EnsureLdap: %v", soPath, err)
	}

	out, err := exec.Command(php, "-m").Output()
	if err != nil {
		t.Fatalf("php -m: %v", err)
	}
	modules := string(out)
	t.Logf("php -m output:\n%s", modules)
	found := false
	for _, line := range strings.Split(modules, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "ldap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected \"ldap\" in php -m output, got:\n%s", modules)
	}

	iniData, err := os.ReadFile(iniPath)
	if err != nil {
		t.Fatalf("reading php.ini after EnsureLdap: %v", err)
	}
	iniContent := string(iniData)
	t.Logf("php.ini after EnsureLdap:\n%s", iniContent)
	extLines := 0
	for _, line := range strings.Split(iniContent, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "extension=") && strings.Contains(l, "ldap.so") {
			extLines++
		}
	}
	if extLines != 1 {
		t.Fatalf("expected exactly one active ldap.so extension line in php.ini, got %d:\n%s", extLines, iniContent)
	}

	t.Logf("SUCCESS: PHP %s + built ldap.so verified end to end (total elapsed %s)", e2eSourceVersion, time.Since(copyStart))
}

// copyExecutable copies src to dst and marks dst executable (0755),
// without relying on the source file's own mode bits.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
