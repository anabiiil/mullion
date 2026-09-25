//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertBlock(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, ".zprofile")
	block := renderBlock([]string{"/home/x/.mullion/bin", "/home/x/.mullion/php/current"})

	// Insert into a missing file.
	changed, err := upsertBlock(profile, block)
	if err != nil || !changed {
		t.Fatalf("first upsert: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(profile)
	if !strings.Contains(string(data), pathBeginMarker) || !strings.Contains(string(data), "php/current") {
		t.Fatalf("block not written: %q", data)
	}
	if !strings.Contains(string(data), `export PHPRC=`) {
		t.Fatalf("PHPRC line missing: %q", data)
	}

	// Re-inserting the same block is a no-op.
	changed, err = upsertBlock(profile, block)
	if err != nil || changed {
		t.Fatalf("second upsert should be a no-op: changed=%v err=%v", changed, err)
	}

	// User content around the block survives an update.
	os.WriteFile(profile, []byte("# mine\n"+string(data)), 0o644)
	newBlock := renderBlock([]string{"/other/bin"})
	if _, err := upsertBlock(profile, newBlock); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(profile)
	if !strings.Contains(string(data), "# mine") || !strings.Contains(string(data), "/other/bin") ||
		strings.Contains(string(data), "php/current") {
		t.Fatalf("update failed: %q", data)
	}
	if strings.Count(string(data), pathBeginMarker) != 1 {
		t.Fatalf("duplicate blocks: %q", data)
	}

	// Removal strips the block and keeps the user's line.
	if _, err := upsertBlock(profile, ""); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(profile)
	if strings.Contains(string(data), pathBeginMarker) || !strings.Contains(string(data), "# mine") {
		t.Fatalf("removal failed: %q", data)
	}
}
