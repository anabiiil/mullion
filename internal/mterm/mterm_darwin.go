//go:build darwin

package mterm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const bundleName = AppName + ".app"

// Overridable for tests: the folders Find looks in (before asking
// Spotlight), the Spotlight lookup itself, and where Install puts the
// app ("" = /Applications, or ~/Applications when that isn't writable).
var (
	searchDirs = func() []string {
		dirs := []string{"/Applications"}
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "Applications"))
		}
		return dirs
	}
	spotlight = func() []string {
		out, err := exec.Command("mdfind", "kMDItemCFBundleIdentifier == '"+BundleID+"'").Output()
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}
	InstallDir = ""
)

// Find locates an installed "Mullion Terminal.app".
func Find() (string, bool) {
	for _, d := range searchDirs() {
		if p := filepath.Join(d, bundleName); isBundle(p) {
			return p, true
		}
	}
	for _, p := range spotlight() {
		// Skip copies inside a mounted installer dmg or the Trash.
		if p == "" || strings.HasPrefix(p, "/Volumes/Mullion Terminal") || strings.Contains(p, "/.Trash/") {
			continue
		}
		if strings.HasSuffix(p, ".app") && isBundle(p) {
			return p, true
		}
	}
	return "", false
}

func isBundle(p string) bool {
	info, err := os.Stat(filepath.Join(p, "Contents", "Info.plist"))
	return err == nil && !info.IsDir()
}

var shortVersionRe = regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]+)</string>`)

// InstalledVersion reads the bundle's CFBundleShortVersionString ("" if
// it can't be read). electron-builder writes an XML plist.
func InstalledVersion(app string) string {
	b, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	if m := shortVersionRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// installFile unpacks the downloaded zip and swaps the app into place.
// ditto, not a Go unzipper: the Electron bundle is full of framework
// symlinks that must survive extraction or the app won't launch. The
// zip is extracted next to the target so the swap is one rename; an
// existing copy is replaced in place when it lives somewhere writable.
func installFile(ctx context.Context, zip, _ string) error {
	dir, err := targetDir()
	if err != nil {
		return err
	}
	target := filepath.Join(dir, bundleName)

	stage, err := os.MkdirTemp(dir, ".mullion-terminal-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if out, err := exec.CommandContext(ctx, "ditto", "-x", "-k", zip, stage).CombinedOutput(); err != nil {
		return fmt.Errorf("extracting Mullion Terminal: %v: %s", err, strings.TrimSpace(string(out)))
	}
	extracted := filepath.Join(stage, bundleName)
	if !isBundle(extracted) {
		return fmt.Errorf("the download did not contain %s", bundleName)
	}

	// Move the old copy aside first so a failed rename can put it back.
	// A running copy keeps working: macOS serves the unlinked inode.
	old := ""
	if _, err := os.Lstat(target); err == nil {
		old = filepath.Join(stage, "old-"+bundleName)
		if err := os.Rename(target, old); err != nil {
			return fmt.Errorf("replacing the existing %s: %w", target, err)
		}
	}
	if err := os.Rename(extracted, target); err != nil {
		if old != "" {
			os.Rename(old, target)
		}
		return err
	}
	// Go's HTTP client doesn't set the quarantine flag, but clear it
	// anyway in case the bundle carried one inside the zip.
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", target).Run()
	return nil
}

// targetDir is InstallDir when set; else the folder of an existing copy
// when writable; else /Applications when writable; else ~/Applications.
func targetDir() (string, error) {
	if InstallDir != "" {
		return InstallDir, os.MkdirAll(InstallDir, 0o755)
	}
	if p, ok := Find(); ok && writable(filepath.Dir(p)) {
		return filepath.Dir(p), nil
	}
	if writable("/Applications") {
		return "/Applications", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Applications")
	return dir, os.MkdirAll(dir, 0o755)
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".mullion-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// launch hands dir to the app through LaunchServices, which delivers it
// as an open-file event — a new tab when the app is already running.
func launch(app, dir string) error {
	args := []string{"-a", app}
	if dir != "" {
		args = append(args, dir)
	}
	if out, err := exec.Command("open", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("opening Mullion Terminal: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
