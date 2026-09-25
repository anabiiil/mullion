package tty

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestIsTerminal_DevNull guards against the ModeCharDevice regression:
// /dev/null is a character device but not a terminal, and this package
// exists specifically to tell the two apart.
func TestIsTerminal_DevNull(t *testing.T) {
	name := "/dev/null"
	if runtime.GOOS == "windows" {
		name = "NUL"
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()

	if IsTerminal(f) {
		t.Errorf("IsTerminal(%s) = true, want false", name)
	}
}

func TestIsTerminal_RegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty-test.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()

	if IsTerminal(f) {
		t.Errorf("IsTerminal(regular file) = true, want false")
	}
}
