package pgadmin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
	"pm/internal/postgres"
)

// TestEndToEnd installs pgAdmin (unless MULLION_PGADMIN_E2E_HOME already
// has it) and pre-registers the Mullion server — it never opens the GUI.
// Skipped unless MULLION_PGADMIN_E2E=1. MULLION_PGADMIN_E2E_HOME is
// REQUIRED: it becomes HOME (and APPDATA on Windows) so pgAdmin's
// config database is created there, never in the real profile; the
// Mullion home is <it>/.mullion. MULLION_PGADMIN_E2E_VERSION picks the
// PostgreSQL package (default: the newest installed there, else 17.11).
func TestEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_PGADMIN_E2E") != "1" {
		t.Skip("set MULLION_PGADMIN_E2E=1 to run (downloads pgAdmin 4)")
	}
	home := os.Getenv("MULLION_PGADMIN_E2E_HOME")
	if home == "" {
		t.Fatal("MULLION_PGADMIN_E2E_HOME is required (it replaces HOME)")
	}
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	}
	paths := pmdir.Paths{Home: filepath.Join(home, ".mullion")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if !Installed(paths) {
		version := os.Getenv("MULLION_PGADMIN_E2E_VERSION")
		if v := postgres.Installed(paths); version == "" && len(v) > 0 {
			version = v[len(v)-1]
		}
		if version == "" {
			version = "17.11"
		}
		start := time.Now()
		if err := Install(ctx, paths, version); err != nil {
			t.Fatal(err)
		}
		t.Logf("installed from %s in %s", version, time.Since(start).Round(time.Second))
	} else if err := verifySignature(current(paths).bundle); err != nil {
		t.Fatal(err)
	}
	if !Installed(paths) {
		t.Fatal("not installed")
	}
	t.Logf("pgAdmin %s", Version(paths))

	db, err := configDB()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(db, home) {
		t.Fatalf("config DB %s is outside the test home", db)
	}
	count := func() string {
		sqlite, err := exec.LookPath("sqlite3")
		if err != nil {
			return "?"
		}
		out, err := exec.Command(sqlite, db,
			"SELECT count(*) FROM server s JOIN servergroup g ON g.id = s.servergroup_id "+
				"WHERE s.name = '"+ServerName+"' AND g.name = '"+GroupName+"' "+
				"AND s.host = '127.0.0.1' AND s.port = 5432 AND s.username = 'postgres'").CombinedOutput()
		if err != nil {
			t.Fatalf("sqlite3: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	for i, step := range []string{"first", "marker", "dump check"} {
		if step == "dump check" {
			os.Remove(markerFile(paths)) // force the dump-servers path
		}
		start := time.Now()
		if err := Register(paths); err != nil {
			t.Fatalf("%s Register: %v", step, err)
		}
		n := count()
		t.Logf("Register #%d (%s): %s, servers named %q: %s", i+1, step, time.Since(start).Round(time.Millisecond), ServerName, n)
		if n != "1" && n != "?" {
			t.Fatalf("%s: want exactly one registered server, got %s", step, n)
		}
	}
	if b, err := os.ReadFile(PassFile(paths)); err != nil || !strings.HasPrefix(string(b), "127.0.0.1:5432:*:postgres:") {
		t.Fatalf("passfile = %q, %v", b, err)
	}
	if runtime.GOOS == "darwin" {
		// Nothing may have been written into the signed bundle.
		var pyc []string
		filepath.WalkDir(current(paths).bundle, func(p string, d os.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(p, ".pyc") {
				pyc = append(pyc, p)
			}
			return nil
		})
		if len(pyc) > 0 {
			t.Fatalf("%d .pyc files written into the bundle, e.g. %s", len(pyc), pyc[0])
		}
	}
}
