package postgres

import (
	"context"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// TestEndToEnd downloads a real PostgreSQL and drives the whole
// lifecycle. It is skipped unless MULLION_PG_E2E=1; MULLION_PG_E2E_HOME
// picks the Mullion home to use (default: a temp dir), so repeated runs
// can reuse the download. Port 5432 must be free.
func TestEndToEnd(t *testing.T) {
	if os.Getenv("MULLION_PG_E2E") != "1" {
		t.Skip("set MULLION_PG_E2E=1 to run (downloads PostgreSQL)")
	}
	useE2EPort(t, "MULLION_PG_E2E_PORT")
	if Running() {
		t.Fatalf("port %d is already in use", Port)
	}
	home := os.Getenv("MULLION_PG_E2E_HOME")
	if home == "" {
		home = t.TempDir()
	}
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	Password = ""
	t.Cleanup(func() { Password = "" })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	version := os.Getenv("MULLION_PG_E2E_VERSION")
	if version == "" {
		var err error
		if version, err = ResolveVersion(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("version %s", version)
	if err := Install(ctx, paths, version); err != nil {
		t.Fatal(err)
	}
	if got := Installed(paths); !slices.Contains(got, version) {
		t.Fatalf("Installed = %v", got)
	}
	if err := EnsureInitialized(paths, version); err != nil {
		t.Fatal(err)
	}
	if !DataInitialized(paths, version) {
		t.Fatal("data dir not initialized")
	}

	if err := Start(paths, version); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Stop(paths, version); err != nil {
			t.Errorf("Stop: %v", err)
		}
		if Running() {
			t.Errorf("port %d still open after Stop", Port)
		}
	})
	if !Running() {
		t.Fatal("not running after Start")
	}

	const db = "mullion_e2e"
	if err := CreateDatabase(paths, version, db); err != nil {
		t.Fatal(err)
	}
	if err := CreateDatabase(paths, version, db); err != nil {
		t.Fatalf("second create should be a no-op: %v", err)
	}
	dbs, err := UserDatabases(paths, version)
	if err != nil || !slices.Contains(dbs, db) {
		t.Fatalf("UserDatabases = %v, %v", dbs, err)
	}

	if err := SetPassword(paths, version, "se'cret"); err != nil {
		t.Fatal(err)
	}
	if Password != "se'cret" {
		t.Fatalf("Password = %q", Password)
	}
	if dbs, err := UserDatabases(paths, version); err != nil || !slices.Contains(dbs, db) {
		t.Fatalf("with password: UserDatabases = %v, %v", dbs, err)
	}
	Password = ""
	if _, err := UserDatabases(paths, version); err == nil {
		t.Fatal("login without the password should fail once one is set")
	}
	Password = "se'cret"
	if err := SetPassword(paths, version, ""); err != nil {
		t.Fatal(err)
	}
	if dbs, err := UserDatabases(paths, version); err != nil || !slices.Contains(dbs, db) {
		t.Fatalf("back to trust: UserDatabases = %v, %v", dbs, err)
	}

	if err := DropDatabase(paths, version, db); err != nil {
		t.Fatal(err)
	}
	if dbs, err := UserDatabases(paths, version); err != nil || slices.Contains(dbs, db) {
		t.Fatalf("after drop: UserDatabases = %v, %v", dbs, err)
	}

	// Restart: the pid file, Stop and a second Start must all behave.
	if err := Stop(paths, version); err != nil {
		t.Fatal(err)
	}
	if Running() {
		t.Fatal("still running after Stop")
	}
	if err := Start(paths, version); err != nil {
		t.Fatal(err)
	}
}

// useE2EPort moves Port to the port named by the env variable, when
// set: runs next to a server already on the default port, and
// exercises a non-default one.
func useE2EPort(t *testing.T, env string) {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		return
	}
	p, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", env, v, err)
	}
	old := Port
	Port = p
	t.Cleanup(func() { Port = old })
}
