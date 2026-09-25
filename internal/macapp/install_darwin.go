//go:build darwin

package macapp

import (
	"fmt"
	"os"
	"path/filepath"
)

// Install extracts the embedded Mullion.app bundle into /Applications
// (or ~/Applications when /Applications isn't writable by us) and
// returns the path it installed to. It's a silent no-op — ("", nil) —
// when this build has no embedded app, e.g. a dev build that skipped
// macapp/build.sh.
//
// The bundle is extracted into a temp dir next to the target first, so
// the swap is a single rename: any previous Mullion.app at the target
// is removed right before that rename (only ever a path literally
// named Mullion.app), and if the app happens to be running, macOS is
// fine with that — it keeps serving the old, now-unlinked inode.
func Install() (string, error) {
	if !Available() {
		return "", nil
	}
	f, err := bundleFS.Open(tarballPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	dir, err := targetDir()
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, "Mullion.app")

	tmp, err := os.MkdirTemp(dir, ".mullion-app-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	if err := extractTarGz(f, tmp); err != nil {
		return "", err
	}
	extracted := filepath.Join(tmp, "Mullion.app")
	if info, err := os.Stat(extracted); err != nil || !info.IsDir() {
		return "", fmt.Errorf("macapp: bundle did not contain Mullion.app")
	}

	removeAppBundle(target)
	if err := os.Rename(extracted, target); err != nil {
		return "", err
	}
	return target, nil
}

// Remove deletes any installed copy of Mullion.app from both possible
// install locations.
func Remove() error {
	removeAppBundle("/Applications/Mullion.app")
	if home, err := os.UserHomeDir(); err == nil {
		removeAppBundle(filepath.Join(home, "Applications", "Mullion.app"))
	}
	return nil
}

// removeAppBundle deletes path, but only when it's literally named
// Mullion.app — a defensive check against ever recursing into the
// wrong directory.
func removeAppBundle(path string) {
	if filepath.Base(path) != "Mullion.app" {
		return
	}
	os.RemoveAll(path)
}

// targetDir picks /Applications when we can write to it, else
// ~/Applications (created on demand).
func targetDir() (string, error) {
	if writable("/Applications") {
		return "/Applications", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// writable reports whether dir can be written to by creating and
// immediately removing a throwaway file in it.
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
