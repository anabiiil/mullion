//go:build windows

package mterm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFind(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	old := searchDirs
	searchDirs = func() []string { return []string{a, b} }
	t.Cleanup(func() { searchDirs = old })

	if p, ok := Find(); ok {
		t.Fatalf("found %s in empty dirs", p)
	}
	// A folder named like the exe isn't the app.
	os.MkdirAll(filepath.Join(a, exeName), 0o755)
	if _, ok := Find(); ok {
		t.Fatal("a directory counted as installed")
	}
	want := filepath.Join(b, exeName)
	if err := os.WriteFile(want, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if p, ok := Find(); !ok || p != want {
		t.Fatalf("Find() = %q, %v; want %q", p, ok, want)
	}
	if err := Open(filepath.Join(b, "missing")); err == nil {
		t.Error("Open accepted a missing folder")
	}
}
