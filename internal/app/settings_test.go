package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	paths := pmdir.Paths{Home: filepath.Join(t.TempDir(), "mullion")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	return &App{Paths: paths, State: mustLoadState(t, paths), skipApply: true}
}

func TestSetBackupDirDefault(t *testing.T) {
	a := newTestApp(t)
	a.State.Config.BackupDir = "/somewhere/custom"
	a.Paths.Backups = "/somewhere/custom"

	if err := a.SetBackupDir(""); err != nil {
		t.Fatalf("SetBackupDir(\"\") = %v", err)
	}
	if a.State.Config.BackupDir != "" {
		t.Errorf("Config.BackupDir = %q, want empty", a.State.Config.BackupDir)
	}
	if a.Paths.Backups != "" {
		t.Errorf("Paths.Backups = %q, want empty", a.Paths.Backups)
	}
	if got := a.Paths.BackupsDir(); got != a.Paths.Home+"-Backups" {
		t.Errorf("BackupsDir() = %q, want the default", got)
	}
}

func TestSetBackupDirCustom(t *testing.T) {
	a := newTestApp(t)
	dir := filepath.Join(t.TempDir(), "my-backups")

	if err := a.SetBackupDir(dir); err != nil {
		t.Fatalf("SetBackupDir(%q) = %v", dir, err)
	}
	if a.State.Config.BackupDir != dir {
		t.Errorf("Config.BackupDir = %q, want %q", a.State.Config.BackupDir, dir)
	}
	if a.Paths.Backups != dir {
		t.Errorf("Paths.Backups = %q, want %q", a.Paths.Backups, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("SetBackupDir did not create %q", dir)
	}

	// Persisted to disk too.
	reloaded, err := config.Load(a.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Config.BackupDir != dir {
		t.Errorf("persisted BackupDir = %q, want %q", reloaded.Config.BackupDir, dir)
	}
}

func TestSetBackupDirExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory available")
	}
	a := newTestApp(t)
	sub := "mullion-settings-test-" + t.Name()
	defer os.RemoveAll(filepath.Join(home, sub))

	if err := a.SetBackupDir("~/" + sub); err != nil {
		t.Fatalf("SetBackupDir(~/...) = %v", err)
	}
	want := filepath.Join(home, sub)
	if a.State.Config.BackupDir != want {
		t.Errorf("Config.BackupDir = %q, want %q", a.State.Config.BackupDir, want)
	}
}

func TestSetBackupDirRejectsRelative(t *testing.T) {
	a := newTestApp(t)
	if err := a.SetBackupDir("relative/path"); err == nil {
		t.Error("SetBackupDir with a relative path should fail")
	}
}

func TestSetBackupDirRejectsRoot(t *testing.T) {
	a := newTestApp(t)
	root := string(filepath.Separator)
	if err := a.SetBackupDir(root); err == nil {
		t.Error("SetBackupDir with the filesystem root should fail")
	}
}

func TestSetBackupDirRejectsInsideHome(t *testing.T) {
	a := newTestApp(t)
	inside := filepath.Join(a.Paths.Home, "backups")
	if err := a.SetBackupDir(inside); err == nil {
		t.Error("SetBackupDir inside the install root should fail")
	}
	if err := a.SetBackupDir(a.Paths.Home); err == nil {
		t.Error("SetBackupDir AT the install root should fail")
	}
}

func TestSetBackupDirDoesNotMoveExistingBackups(t *testing.T) {
	a := newTestApp(t)
	oldDir := a.Paths.BackupsDir()
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(oldDir, "old-backup.txt")
	if err := os.WriteFile(marker, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	newDir := filepath.Join(t.TempDir(), "new-backups")
	if err := a.SetBackupDir(newDir); err != nil {
		t.Fatalf("SetBackupDir: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("old backup file was moved/removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "old-backup.txt")); err == nil {
		t.Errorf("SetBackupDir should not copy old backups into the new dir")
	}
}

func TestBackupDirInfo(t *testing.T) {
	a := newTestApp(t)

	dir, isDefault, _ := a.BackupDirInfo()
	if !isDefault {
		t.Error("BackupDirInfo isDefault = false, want true before SetBackupDir")
	}
	if dir != a.Paths.Home+"-Backups" {
		t.Errorf("BackupDirInfo dir = %q, want the default", dir)
	}

	custom := filepath.Join(t.TempDir(), "custom-backups")
	if err := a.SetBackupDir(custom); err != nil {
		t.Fatal(err)
	}
	dir, isDefault, free := a.BackupDirInfo()
	if isDefault {
		t.Error("BackupDirInfo isDefault = true, want false after SetBackupDir")
	}
	if dir != custom {
		t.Errorf("BackupDirInfo dir = %q, want %q", dir, custom)
	}
	if free == 0 {
		t.Error("BackupDirInfo freeBytes = 0, want a positive number of free bytes")
	}
}

func TestSetSitePinned(t *testing.T) {
	a := newTestApp(t)
	a.State.AddSite(config.Site{Name: "blog", Path: "/tmp/blog"})

	if err := a.SetSitePinned("blog", true); err != nil {
		t.Fatalf("SetSitePinned: %v", err)
	}
	if !a.State.FindSite("blog").Pinned {
		t.Error("site should be pinned")
	}

	reloaded, err := config.Load(a.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.FindSite("blog").Pinned {
		t.Error("pinned flag was not persisted")
	}

	if err := a.SetSitePinned("blog", false); err != nil {
		t.Fatalf("SetSitePinned(false): %v", err)
	}
	if a.State.FindSite("blog").Pinned {
		t.Error("site should be unpinned")
	}
}

func TestSetSitePinnedUnknownSite(t *testing.T) {
	a := newTestApp(t)
	if err := a.SetSitePinned("nope", true); err == nil {
		t.Error("SetSitePinned on an unknown site should fail")
	}
}

func TestNodeVersionsWithNpmEmpty(t *testing.T) {
	a := newTestApp(t)
	if got := a.NodeVersionsWithNpm(); len(got) != 0 {
		t.Errorf("NodeVersionsWithNpm on a fresh install = %v, want empty", got)
	}
}

func TestNodeVersionsWithNpm(t *testing.T) {
	a := newTestApp(t)
	a.State.Config.GlobalNode = "22.12.0"

	for _, v := range []string{"20.18.0", "22.12.0"} {
		npmDir := filepath.Join(a.Paths.NodeVersionDir(v), "lib", "node_modules", "npm")
		if runtime.GOOS == "windows" {
			npmDir = filepath.Join(a.Paths.NodeVersionDir(v), "node_modules", "npm")
		}
		if err := os.MkdirAll(npmDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(npmDir, "package.json"), []byte(`{"version": "10.9.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := a.NodeVersionsWithNpm()
	if len(got) != 2 {
		t.Fatalf("NodeVersionsWithNpm = %v, want 2 entries", got)
	}
	byNode := map[string]NodeNpm{}
	for _, n := range got {
		byNode[n.Node] = n
	}
	if byNode["22.12.0"].Npm != "10.9.0" || !byNode["22.12.0"].Default {
		t.Errorf("22.12.0 entry = %+v, want npm 10.9.0 and Default true", byNode["22.12.0"])
	}
	if byNode["20.18.0"].Default {
		t.Error("20.18.0 should not be marked Default")
	}
}

func TestSitesInfo(t *testing.T) {
	a := newTestApp(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.State.AddSite(config.Site{Name: "static-site", Path: dir, Pinned: true})

	got := a.SitesInfo()
	if len(got) != 1 {
		t.Fatalf("SitesInfo = %v, want 1 entry", got)
	}
	if got[0].Name != "static-site" || !got[0].Pinned {
		t.Errorf("SitesInfo[0] = %+v, want name static-site, pinned true", got[0])
	}
	if got[0].Detect.Icon != "static" {
		t.Errorf("SitesInfo[0].Detect.Icon = %q, want static", got[0].Detect.Icon)
	}
}
