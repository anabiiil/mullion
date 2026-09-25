package mongodb

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// e2eHome returns an isolated Mullion home for the end-to-end tests:
// MULLION_MONGO_E2E_HOME when set (so downloads are reused between
// runs), a fresh temp dir otherwise. The real ~/.mullion is never used.
func e2eHome(t *testing.T) pmdir.Paths {
	t.Helper()
	if os.Getenv("MULLION_MONGO_E2E") != "1" {
		t.Skip("set MULLION_MONGO_E2E=1 to run (downloads MongoDB and mongosh)")
	}
	home := os.Getenv("MULLION_MONGO_E2E_HOME")
	if home == "" {
		home = t.TempDir()
	}
	// mongosh keeps its own state under $HOME/.mongodb — keep it in the
	// sandbox too.
	t.Setenv("HOME", filepath.Join(home, "user-home"))
	t.Setenv("USERPROFILE", filepath.Join(home, "user-home"))
	paths := pmdir.Paths{Home: filepath.Join(home, "mullion")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestE2EInstallStartCreateDropStop(t *testing.T) {
	paths := e2eHome(t)
	useE2EPort(t, "MULLION_MONGO_E2E_PORT")
	if Running() {
		t.Fatalf("port %d is already in use — refusing to test against someone else's server", Port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	series, err := AvailableSeries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("available series: %v", series)
	version, err := ResolveVersion(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default resolves to %s", version)
	if seriesOf(version) != DefaultSeries {
		t.Fatalf("default %s is not in series %s", version, DefaultSeries)
	}
	if err := Install(ctx, paths, version); err != nil {
		t.Fatal(err)
	}
	if got := Installed(paths); !slices.Contains(got, version) {
		t.Fatalf("Installed() = %v, missing %s", got, version)
	}
	if MongoshPath(paths) == "" {
		t.Fatal("mongosh not installed")
	}
	t.Logf("mongosh: %s", MongoshPath(paths))
	// A second Install is a no-op.
	if err := Install(ctx, paths, version); err != nil {
		t.Fatal(err)
	}

	if err := EnsureInitialized(paths, version); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := Start(paths, version); err != nil {
		t.Fatal(err)
	}
	t.Logf("started in %v", time.Since(t0).Round(time.Millisecond))
	stopped := false
	defer func() {
		if !stopped {
			_ = Stop(paths, version)
		}
	}()
	if !Running() || !DataInitialized(paths) {
		t.Fatalf("Running=%v DataInitialized=%v after Start", Running(), DataInitialized(paths))
	}

	if err := CreateDatabase(paths, "mullion_e2e"); err != nil {
		t.Fatal(err)
	}
	if err := CreateDatabase(paths, "mullion_e2e"); err != nil {
		t.Fatalf("second CreateDatabase: %v", err)
	}
	dbs, err := UserDatabases(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("user databases: %v", dbs)
	if !slices.Contains(dbs, "mullion_e2e") {
		t.Fatalf("UserDatabases() = %v, missing mullion_e2e", dbs)
	}
	for _, sys := range []string{"admin", "local", "config"} {
		if slices.Contains(dbs, sys) {
			t.Fatalf("system database %s listed", sys)
		}
	}
	if err := DropDatabase(paths, "admin"); err == nil {
		t.Fatal("dropping admin was allowed")
	}
	if err := DropDatabase(paths, "mullion_e2e"); err != nil {
		t.Fatal(err)
	}
	if dbs, err = UserDatabases(paths); err != nil || slices.Contains(dbs, "mullion_e2e") {
		t.Fatalf("after drop: %v, %v", dbs, err)
	}

	t0 = time.Now()
	if err := Stop(paths, version); err != nil {
		t.Fatal(err)
	}
	stopped = true
	t.Logf("stopped in %v", time.Since(t0).Round(time.Millisecond))
	if Running() {
		t.Fatalf("port %d still open after Stop", Port)
	}
	if _, err := os.Stat(pidFile(paths)); err == nil {
		t.Fatal("pid file left behind")
	}
	// Restart on the existing data dir, then stop again.
	if err := Start(paths, version); err != nil {
		t.Fatal(err)
	}
	stopped = false
	if err := Stop(paths, version); err != nil {
		t.Fatal(err)
	}
	stopped = true
	if Running() {
		t.Fatalf("port %d still open after second Stop", Port)
	}
}

// TestE2EWindowsServerRangedExtract exercises the Windows install path's
// partial download (mongod.exe only, out of a ~0.8 GB zip) from any OS.
func TestE2EWindowsServerRangedExtract(t *testing.T) {
	paths := e2eHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dest := filepath.Join(paths.TmpDir(), "win-ranged")
	os.RemoveAll(dest)
	defer os.RemoveAll(dest)
	url := "https://fastdl.mongodb.org/windows/mongodb-windows-x86_64-8.0.32.zip"
	err := extractRemoteZip(ctx, url, dest, func(name string) bool {
		dir, base := filepath.Split(filepath.FromSlash(name))
		return filepath.Base(filepath.Clean(dir)) == "bin" && contains([]string{"mongod.exe"}, base)
	})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := findDirWithName(dest, "mongod.exe")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(inner, "bin", "mongod.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 10<<20 || string(data[:2]) != "MZ" {
		t.Fatalf("mongod.exe looks wrong: %d bytes", len(data))
	}
	entries, _ := os.ReadDir(filepath.Join(inner, "bin"))
	if len(entries) != 1 {
		t.Fatalf("extracted more than mongod.exe: %v", entries)
	}
	t.Logf("mongod.exe: %d bytes", len(data))
}

func findDirWithName(staging, exe string) (string, error) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(staging, e.Name(), "bin", exe)); err == nil {
			return filepath.Join(staging, e.Name()), nil
		}
	}
	return "", os.ErrNotExist
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
