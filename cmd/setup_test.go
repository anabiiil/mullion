package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"pm/internal/app"
	"pm/internal/pmdir"
)

func TestNormalizeBackupPathStripsQuotes(t *testing.T) {
	abs := filepath.Join(os.TempDir(), "quoted-backups")
	for _, raw := range []string{
		`"` + abs + `"`,
		`'` + abs + `'`,
	} {
		if got := normalizeBackupPath(raw); got != filepath.Clean(abs) {
			t.Errorf("normalizeBackupPath(%q) = %q, want %q", raw, got, filepath.Clean(abs))
		}
	}
}

func TestNormalizeBackupPathExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}
	cases := map[string]string{
		"~":         home,
		"~/backups": filepath.Join(home, "backups"),
		`~\backups`: filepath.Join(home, "backups"),
	}
	for raw, want := range cases {
		if got := normalizeBackupPath(raw); got != filepath.Clean(want) {
			t.Errorf("normalizeBackupPath(%q) = %q, want %q", raw, got, filepath.Clean(want))
		}
	}
}

func TestNormalizeBackupPathMakesRelativeAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := normalizeBackupPath(filepath.Join("relative", "backups"))
	want := filepath.Clean(filepath.Join(cwd, "relative", "backups"))
	if got != want {
		t.Errorf("normalizeBackupPath(relative) = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("normalizeBackupPath(relative) = %q, want an absolute path", got)
	}
}

func TestNormalizeBackupPathExpandsDollarEnv(t *testing.T) {
	t.Setenv("MULLION_TEST_BACKUP_DIR", "env-backups")
	got := normalizeBackupPath(filepath.Join(os.TempDir(), "$MULLION_TEST_BACKUP_DIR"))
	want := filepath.Clean(filepath.Join(os.TempDir(), "env-backups"))
	if got != want {
		t.Errorf("normalizeBackupPath($VAR) = %q, want %q", got, want)
	}
}

// expandWindowsEnv itself has no OS dependency (it's pure string
// substitution against the process environment), so it's exercised
// directly on every platform.
func TestExpandWindowsEnv(t *testing.T) {
	t.Setenv("MULLION_TEST_USERPROFILE", `C:\Users\dev`)
	cases := map[string]string{
		`%MULLION_TEST_USERPROFILE%\backups`: `C:\Users\dev\backups`,
		`%NOT_A_REAL_VAR%\backups`:           `%NOT_A_REAL_VAR%\backups`,
		`no percent signs here`:              `no percent signs here`,
	}
	for raw, want := range cases {
		if got := expandWindowsEnv(raw); got != want {
			t.Errorf("expandWindowsEnv(%q) = %q, want %q", raw, got, want)
		}
	}
}

// normalizeBackupPath only runs %VAR% expansion when GOOS is windows —
// elsewhere a %VAR%-shaped string is just an unusual (but literal)
// directory name, same as any other OS leaves it.
func TestNormalizeBackupPathWindowsEnvVar(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("%VAR% expansion only applies on windows")
	}
	t.Setenv("MULLION_TEST_USERPROFILE", `C:\Users\dev`)
	got := normalizeBackupPath(`%MULLION_TEST_USERPROFILE%\backups`)
	want := filepath.Clean(`C:\Users\dev\backups`)
	if got != want {
		t.Errorf("normalizeBackupPath(%%VAR%%) = %q, want %q", got, want)
	}
}

func TestNormalizeBackupPathLeavesPercentVarOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by TestNormalizeBackupPathWindowsEnvVar on windows")
	}
	t.Setenv("MULLION_TEST_USERPROFILE", "/home/dev")
	got := normalizeBackupPath("%MULLION_TEST_USERPROFILE%/backups")
	if got == filepath.Clean("/home/dev/backups") {
		t.Errorf("normalizeBackupPath(%%VAR%%) expanded %%VAR%% outside windows")
	}
}

// restoreFilesIn picks what a restore should load from an export
// folder: all-databases.sql alone when present, else every *.sql file
// sorted by name (the per-database dumps importFromExistingMysql now
// writes).

func TestRestoreFilesInPrefersCombinedFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"all-databases.sql", "shop.sql", "blog.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("-- dump"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := restoreFilesIn(dir)
	if err != nil {
		t.Fatalf("restoreFilesIn: %v", err)
	}
	want := []string{filepath.Join(dir, "all-databases.sql")}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("restoreFilesIn(%q) = %v, want %v", dir, got, want)
	}
}

func TestRestoreFilesInSortsPerDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"shop.sql", "blog.sql", "cms.sql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("-- dump"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := restoreFilesIn(dir)
	if err != nil {
		t.Fatalf("restoreFilesIn: %v", err)
	}
	want := []string{
		filepath.Join(dir, "blog.sql"),
		filepath.Join(dir, "cms.sql"),
		filepath.Join(dir, "shop.sql"),
	}
	if len(got) != len(want) {
		t.Fatalf("restoreFilesIn(%q) = %v, want %v", dir, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("restoreFilesIn(%q)[%d] = %q, want %q", dir, i, got[i], want[i])
		}
	}
}

func TestRestoreFilesInIgnoresNonSqlFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shop.sql"), []byte("-- dump"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := restoreFilesIn(dir)
	if err != nil {
		t.Fatalf("restoreFilesIn: %v", err)
	}
	want := []string{filepath.Join(dir, "shop.sql")}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("restoreFilesIn(%q) = %v, want %v", dir, got, want)
	}
}

func TestRestoreFilesInErrorsWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := restoreFilesIn(dir); err == nil {
		t.Error("restoreFilesIn on an empty folder should fail")
	}
}

// ensureDatadirSpace, like ValidateBackupDirForExport, must skip the
// check entirely when the export's size couldn't be estimated, and
// must not block (go test's stdin isn't a terminal, so
// console.Interactive() is false here) when there IS enough room.

func TestEnsureDatadirSpaceSkipsUnknownEstimate(t *testing.T) {
	a := &app.App{Paths: pmdir.Paths{Home: t.TempDir()}}
	for _, estimate := range []int64{0, -1} {
		if !ensureDatadirSpace(a, estimate, t.TempDir()) {
			t.Errorf("ensureDatadirSpace(estimate=%d) = false, want true (check skipped)", estimate)
		}
	}
}

func TestEnsureDatadirSpaceFitsSmallEstimate(t *testing.T) {
	a := &app.App{Paths: pmdir.Paths{Home: t.TempDir()}}
	if !ensureDatadirSpace(a, 1024, t.TempDir()) {
		t.Error("ensureDatadirSpace(1KB) = false, want true")
	}
}

// Non-interactive (no human to ask), ensureDatadirSpace must warn and
// skip rather than block forever or guess — an impossible estimate
// (no real volume has an exabyte free) exercises exactly that path.
func TestEnsureDatadirSpaceSkipsOnNonInteractiveShortfall(t *testing.T) {
	a := &app.App{Paths: pmdir.Paths{Home: t.TempDir()}}
	const huge = int64(1) << 60
	if ensureDatadirSpace(a, huge, t.TempDir()) {
		t.Error("ensureDatadirSpace with an impossible estimate, non-interactive, should return false")
	}
}
