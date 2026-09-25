package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func TestBackupEngineFromName(t *testing.T) {
	cases := []struct {
		name       string
		wantEngine string
		wantOK     bool
	}{
		{"2026-09-24_153000-postgres", "postgres", true},
		{"2026-09-24_153000-mysql", "mysql", true},
		{"2026-09-24_153000-mongo", "mongo", true},
		{"2026-09-24_153000-migrate-8.4.11", "", false},
		{"2026-09-24_153000-migrate-mysql", "mysql", true}, // ends in -mysql, correctly recognized
		{"not-a-backup", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		engine, ok := backupEngineFromName(c.name)
		if engine != c.wantEngine || ok != c.wantOK {
			t.Errorf("backupEngineFromName(%q) = (%q, %v), want (%q, %v)", c.name, engine, ok, c.wantEngine, c.wantOK)
		}
	}
}

func TestNewBackupDirNaming(t *testing.T) {
	paths := pmdir.Paths{Home: filepath.Join(t.TempDir(), "mullion")}
	dir := newBackupDir(paths, "postgres")
	base := filepath.Base(dir)
	engine, ok := backupEngineFromName(base)
	if !ok || engine != "postgres" {
		t.Fatalf("newBackupDir name %q not recognized as a postgres backup", base)
	}
	if filepath.Dir(dir) != paths.BackupsDir() {
		t.Fatalf("newBackupDir = %q, want it under %q", dir, paths.BackupsDir())
	}
}

func TestListBackupFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app.dump", 100)
	write("globals.sql", 50)
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, size := listBackupFiles(dir)
	if len(files) != 2 || files[0] != "app.dump" || files[1] != "globals.sql" {
		t.Fatalf("listBackupFiles files = %v, want [app.dump globals.sql]", files)
	}
	if size != 150 {
		t.Fatalf("listBackupFiles size = %d, want 150", size)
	}

	if files, size := listBackupFiles(filepath.Join(dir, "does-not-exist")); files != nil || size != 0 {
		t.Fatalf("listBackupFiles on a missing dir = %v, %d, want nil, 0", files, size)
	}
}

func TestListBackupsSkipsUnrecognizedAndSortsNewestFirst(t *testing.T) {
	home := t.TempDir()
	paths := pmdir.Paths{Home: filepath.Join(home, "mullion")}
	backups := paths.BackupsDir()
	for _, name := range []string{
		"2026-01-01_000000-postgres",
		"2026-06-01_000000-mysql",
		"2026-03-01_000000-mongo",
		"2026-01-01_000000-migrate-8.4.11", // not a BackupEngine backup — excluded
		"not-a-timestamped-dir",
	} {
		if err := os.MkdirAll(filepath.Join(backups, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file (not a directory) under BackupsDir must be ignored too.
	if err := os.WriteFile(filepath.Join(backups, "README.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &App{Paths: paths, State: mustLoadState(t, paths), skipApply: true}
	got, err := a.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListBackups returned %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Name != "2026-06-01_000000-mysql" || got[1].Name != "2026-03-01_000000-mongo" || got[2].Name != "2026-01-01_000000-postgres" {
		t.Fatalf("ListBackups order = %v, want newest first", []string{got[0].Name, got[1].Name, got[2].Name})
	}
	for _, b := range got {
		if _, err := time.Parse(time.RFC3339, b.Time); err != nil {
			t.Errorf("backup %s: Time %q is not RFC3339: %v", b.Name, b.Time, err)
		}
	}
}

func TestListBackupsNoDirYet(t *testing.T) {
	paths := pmdir.Paths{Home: filepath.Join(t.TempDir(), "mullion")}
	a := &App{Paths: paths, State: mustLoadState(t, paths), skipApply: true}
	got, err := a.ListBackups()
	if err != nil || got != nil {
		t.Fatalf("ListBackups on a fresh install = %v, %v, want nil, nil", got, err)
	}
}

func TestRemoveEngineBinariesSparesDataUnlessAsked(t *testing.T) {
	base := t.TempDir()
	mustDir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(base, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustDir("17.6")
	mustDir("data-17")
	mustDir("data")

	if err := removeEngineBinaries(base, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "17.6")); !os.IsNotExist(err) {
		t.Error("binaries dir should have been removed")
	}
	if _, err := os.Stat(filepath.Join(base, "data-17")); err != nil {
		t.Error("data-17 should have been spared")
	}
	if _, err := os.Stat(filepath.Join(base, "data")); err != nil {
		t.Error("data should have been spared")
	}

	if err := removeEngineBinaries(base, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Error("deleteData=true should remove the whole base directory")
	}
}

func TestRemoveEngineBinariesMissingDirIsNotAnError(t *testing.T) {
	if err := removeEngineBinaries(filepath.Join(t.TempDir(), "does-not-exist"), false); err != nil {
		t.Fatalf("removeEngineBinaries on a missing dir = %v, want nil", err)
	}
}

// mustLoadState gives an App a fresh, empty *config.State for the
// given (not-yet-existing) home directory.
func mustLoadState(t *testing.T, paths pmdir.Paths) *config.State {
	t.Helper()
	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
