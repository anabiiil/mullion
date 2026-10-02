//go:build darwin

package mterm

import (
	"archive/zip"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const testPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
	<key>CFBundleIdentifier</key>
	<string>dev.mullion.terminal</string>
	<key>CFBundleShortVersionString</key>
	<string>%s</string>
</dict></plist>`

// isolate points Find at the given folders only (no Spotlight, so a
// real installed copy is never seen) and Install at installDir.
func isolate(t *testing.T, installDir string, dirs ...string) {
	t.Helper()
	oldDirs, oldSpot, oldInst := searchDirs, spotlight, InstallDir
	searchDirs = func() []string { return dirs }
	spotlight = func() []string { return nil }
	InstallDir = installDir
	t.Cleanup(func() { searchDirs, spotlight, InstallDir = oldDirs, oldSpot, oldInst })
}

func makeBundle(t *testing.T, dir, version string) string {
	t.Helper()
	app := filepath.Join(dir, bundleName)
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(fmt.Sprintf(testPlist, version)), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestFind(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	isolate(t, "", a, b)
	if p, ok := Find(); ok {
		t.Fatalf("found %s in empty dirs", p)
	}
	// An empty folder named like the bundle isn't an app.
	os.MkdirAll(filepath.Join(a, bundleName), 0o755)
	if _, ok := Find(); ok {
		t.Fatal("an empty .app folder counted as installed")
	}
	want := makeBundle(t, b, "0.1.5")
	p, ok := Find()
	if !ok || p != want {
		t.Fatalf("Find() = %q, %v; want %q", p, ok, want)
	}
	if v := InstalledVersion(p); v != "0.1.5" {
		t.Errorf("InstalledVersion = %q", v)
	}
}

func TestFindSpotlight(t *testing.T) {
	dir := t.TempDir()
	isolate(t, "")
	app := makeBundle(t, dir, "0.1.4")
	spotlight = func() []string { return []string{"/Volumes/Mullion Terminal 0.1.4/" + bundleName, app} }
	if p, ok := Find(); !ok || p != app {
		t.Errorf("Find() = %q, %v; want %q", p, ok, app)
	}
}

// writeZip builds a release zip holding a fake app bundle (with a
// symlink, like Electron's frameworks have).
func writeZip(t *testing.T, path, version string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, _ := zw.Create(bundleName + "/Contents/Info.plist")
	fmt.Fprintf(w, testPlist, version)
	w, _ = zw.Create(bundleName + "/Contents/Frameworks/Lib.framework/Versions/A/Lib")
	w.Write([]byte("binary"))
	h := &zip.FileHeader{Name: bundleName + "/Contents/Frameworks/Lib.framework/Lib"}
	h.SetMode(os.ModeSymlink | 0o755)
	w, _ = zw.CreateHeader(h)
	w.Write([]byte("Versions/A/Lib"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestInstall runs the whole install against a fake GitHub (httptest)
// into a temp folder: real ditto, but no network and no /Applications.
func TestInstall(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("no Mullion Terminal build for this architecture")
	}
	zipPath := filepath.Join(t.TempDir(), "release.zip")
	writeZip(t, zipPath, "0.1.6")
	var base string
	withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.1.6","assets":[
				{"name":"Mullion-Terminal-0.1.6-mac-arm64.dmg","browser_download_url":"%[1]s/nope"},
				{"name":"Mullion-Terminal-0.1.6-mac-arm64.zip","browser_download_url":"%[1]s/dl/app.zip"}]}`, base)
		case "/dl/app.zip":
			http.ServeFile(w, r, zipPath)
		default:
			http.NotFound(w, r)
		}
	}))
	base = APIBase

	dest := t.TempDir()
	isolate(t, dest, dest)
	old := makeBundle(t, dest, "0.1.5") // an older copy gets replaced
	os.WriteFile(filepath.Join(old, "stale"), []byte("x"), 0o644)

	var sawDownload, sawInstall bool
	v, err := Install(context.Background(), func(stage string, done, total int64) {
		switch stage {
		case "download":
			sawDownload = sawDownload || done > 0
		case "install":
			sawInstall = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.1.6" || !sawDownload || !sawInstall {
		t.Errorf("version %q, download progress %v, install stage %v", v, sawDownload, sawInstall)
	}
	app := filepath.Join(dest, bundleName)
	if got := InstalledVersion(app); got != "0.1.6" {
		t.Errorf("installed version %q", got)
	}
	if _, err := os.Stat(filepath.Join(app, "stale")); !os.IsNotExist(err) {
		t.Error("the old copy wasn't replaced")
	}
	link := filepath.Join(app, "Contents/Frameworks/Lib.framework/Lib")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink not preserved: %v %v", fi, err)
	}
	entries, _ := os.ReadDir(dest)
	if len(entries) != 1 {
		t.Errorf("staging left behind: %v", entries)
	}
	st := GetStatus(context.Background(), true)
	if !st.Installed || st.Version != "0.1.6" || st.Latest != "0.1.6" || st.Update {
		t.Errorf("status %+v", st)
	}
}

func TestOpenNotInstalled(t *testing.T) {
	isolate(t, "", t.TempDir())
	if err := Open(t.TempDir()); err == nil {
		t.Error("Open succeeded without an installed app")
	}
}
