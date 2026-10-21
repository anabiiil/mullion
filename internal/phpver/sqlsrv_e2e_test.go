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

func TestPdoDblibIniCommentHasNoEquals(t *testing.T) {
	if strings.Contains(pdoDblibIniComment, "=") {
		t.Fatal("the pdo_dblib ini comment must not contain '=' (parseExtLine would read it as a directive)")
	}
}

func TestPreflightSQLServerTools(t *testing.T) {
	if err := preflightSQLServerTools(); err != nil {
		t.Skipf("Command Line Tools not available in this environment: %v", err)
	}
}

// TestEnsureSQLServerEndToEnd exercises the real build: it downloads the
// FreeTDS (~5MB) and php.net (~14MB) source tarballs and compiles
// FreeTDS + ext/pdo_dblib, so it's skipped unless MULLION_SQLSRV_E2E=1.
// Like TestEnsureLdapEndToEnd it never touches the real ~/.mullion — it
// only copies the php binary installed there for e2eSourceVersion into a
// throwaway temp HOME. It then toggles pdo_dblib off and on through
// SetExtension / ListExtensions.
func TestEnsureSQLServerEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_SQLSRV_E2E") != "1" {
		t.Skip("set MULLION_SQLSRV_E2E=1 to run (downloads ~19MB and compiles for ~1-2 min)")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	srcPhp := filepath.Join(realHome, ".mullion", "php", e2eSourceVersion, PhpExeName)
	if _, err := os.Stat(srcPhp); err != nil {
		t.Fatalf("expected %s to already be installed on this machine for the e2e test: %v", srcPhp, err)
	}

	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	verDir := paths.PhpVersionDir(e2eSourceVersion)
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	php := filepath.Join(verDir, PhpExeName)
	if err := copyExecutable(srcPhp, php); err != nil {
		t.Fatal(err)
	}
	iniPath := filepath.Join(verDir, "php.ini")
	if err := os.WriteFile(iniPath, []byte("display_errors=On\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := EnsureSQLServer(context.Background(), paths, e2eSourceVersion); err != nil {
		t.Fatalf("EnsureSQLServer: %v", err)
	}
	t.Logf("EnsureSQLServer completed in %s", time.Since(start))
	if !freetdsReady(freetdsCacheDir(paths, "arm64")) && !freetdsReady(freetdsCacheDir(paths, "amd64")) {
		t.Fatal("FreeTDS cache missing after EnsureSQLServer")
	}

	out, err := exec.Command(php, "-c", iniPath, "-r", `echo implode(",", PDO::getAvailableDrivers());`).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "dblib") {
		t.Fatalf("PDO drivers after EnsureSQLServer: %s (%v)", out, err)
	}
	if !HasSQLServerDriver(paths, e2eSourceVersion) {
		t.Fatal("HasSQLServerDriver = false after EnsureSQLServer")
	}

	// A second run is a no-op, and toggling works through the generic API.
	if err := EnsureSQLServer(context.Background(), paths, e2eSourceVersion); err != nil {
		t.Fatalf("second EnsureSQLServer: %v", err)
	}
	if err := SetExtension(paths, e2eSourceVersion, "pdo_dblib", false); err != nil {
		t.Fatal(err)
	}
	if HasSQLServerDriver(paths, e2eSourceVersion) {
		t.Fatal("still enabled after SetExtension(false)")
	}
	exts, err := ListExtensions(paths, e2eSourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range exts {
		if e.Name == "pdo_dblib" {
			found = true
			if e.Enabled || !e.Toggleable {
				t.Fatalf("disabled pdo_dblib listed as %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("disabled pdo_dblib missing from ListExtensions")
	}
	if err := SetExtension(paths, e2eSourceVersion, "pdo_dblib", true); err != nil {
		t.Fatal(err)
	}
	if !HasSQLServerDriver(paths, e2eSourceVersion) {
		t.Fatal("not enabled after SetExtension(true)")
	}
	ini, _ := os.ReadFile(iniPath)
	t.Logf("php.ini:\n%s", ini)
}
