package diskfree

import (
	"path/filepath"
	"testing"
)

// TestFreePositive checks that Free resolves a real path to a
// positive byte count. It only touches a throwaway temp directory, so
// it's safe to run on any machine (never the real HOME or /etc/hosts).
func TestFreePositive(t *testing.T) {
	free, err := Free(t.TempDir())
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if free == 0 {
		t.Error("Free returned 0, want a positive number of free bytes")
	}
}

// TestFreeNonexistentPath checks that Free still works when the exact
// path doesn't exist yet (e.g. Mullion's MySQL data directory before
// first init) by climbing to the nearest existing ancestor.
func TestFreeNonexistentPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not", "created", "yet")
	free, err := Free(missing)
	if err != nil {
		t.Fatalf("Free(%q): %v", missing, err)
	}
	if free == 0 {
		t.Error("Free returned 0, want a positive number of free bytes")
	}
}

// TestSameVolumeSamePath checks the trivial case: a path is always on
// the same volume as itself.
func TestSameVolumeSamePath(t *testing.T) {
	dir := t.TempDir()
	if !SameVolume(dir, dir) {
		t.Error("SameVolume(dir, dir) = false, want true")
	}
}

// TestSameVolumeNestedUnderSameRoot checks two not-yet-created paths
// that share an existing ancestor (both under the same temp root, the
// way a backup folder and Mullion's own data directory would both sit
// under the same install root on a typical machine).
func TestSameVolumeNestedUnderSameRoot(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "does-not-exist-yet")
	b := filepath.Join(root, "b", "also-missing")
	if !SameVolume(a, b) {
		t.Error("SameVolume for two not-yet-created paths under the same tmp root = false, want true")
	}
}
