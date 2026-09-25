package macapp

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// buildTarGz builds a small in-memory tar.gz with a directory, an
// executable regular file, and a path-traversal entry.
func buildTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	entries := []struct {
		name string
		mode int64
		typ  byte
		body string
	}{
		{"Mullion.app/", 0o755, tar.TypeDir, ""},
		{"Mullion.app/Contents/MacOS/Mullion", 0o755, tar.TypeReg, "#!/bin/sh\necho hi\n"},
		{"../evil", 0o644, tar.TypeReg, "gotcha"},
	}
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.body)),
			Typeflag: e.typ,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write(%s): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	data := buildTarGz(t)
	dest := filepath.Join(t.TempDir(), "dest")

	err := extractTarGz(bytes.NewReader(data), dest)
	if err == nil {
		t.Fatal("expected an error for the ../evil entry, got nil")
	}

	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); statErr == nil {
		t.Fatal("../evil entry escaped destDir")
	}
}

func TestExtractTarGzPreservesModes(t *testing.T) {
	// Build a tarball with no traversal entry so extraction succeeds
	// and we can assert on what landed on disk.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "Mullion.app/", Mode: 0o755, Typeflag: tar.TypeDir}))
	must(tw.WriteHeader(&tar.Header{Name: "Mullion.app/Contents/", Mode: 0o755, Typeflag: tar.TypeDir}))
	body := "#!/bin/sh\necho hi\n"
	must(tw.WriteHeader(&tar.Header{Name: "Mullion.app/Contents/MacOS/Mullion", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte(body))
	must(err)
	must(tw.Close())
	must(gz.Close())

	dest := t.TempDir()
	if err := extractTarGz(bytes.NewReader(buf.Bytes()), dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}

	exe := filepath.Join(dest, "Mullion.app", "Contents", "MacOS", "Mullion")
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat extracted binary: %v", err)
	}
	// Windows has no Unix permission bits (the app only installs on macOS).
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o755 {
		t.Fatalf("extracted binary mode = %o, want 0755", got)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatal("extracted binary is not executable")
	}
}
