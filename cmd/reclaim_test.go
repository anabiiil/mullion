package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/pmdir"
)

// restorePendingDump must consume (and clear) pendingRestore exactly
// once, whatever shape the value has — a export folder (the normal
// case) or a single file (backward compatibility with an older
// Mullion build that saved a combined all-databases.sql path here).
// Neither binaries nor a running server exist in this sandbox, so the
// restore itself fails fast (no network, no real HOME) — this only
// checks the dispatch and bookkeeping, not a real import.

func testApp(t *testing.T) *app.App {
	t.Helper()
	return &app.App{
		Paths: pmdir.Paths{Home: t.TempDir()},
		State: &config.State{Config: config.Config{MySQL: "8.4.5"}},
	}
}

func TestRestorePendingDumpNoop(t *testing.T) {
	pendingRestore = ""
	restorePendingDump(testApp(t)) // must not panic on an empty pending value
	if pendingRestore != "" {
		t.Errorf("pendingRestore = %q after a no-op call, want empty", pendingRestore)
	}
}

func TestRestorePendingDumpClearsForSingleFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "all-databases.sql")
	if err := os.WriteFile(file, []byte("-- dump"), 0o644); err != nil {
		t.Fatal(err)
	}
	pendingRestore = file
	t.Cleanup(func() { pendingRestore = "" })

	restorePendingDump(testApp(t))

	if pendingRestore != "" {
		t.Errorf("pendingRestore = %q after restorePendingDump, want cleared", pendingRestore)
	}
}

func TestRestorePendingDumpClearsForFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shop.sql"), []byte("-- dump"), 0o644); err != nil {
		t.Fatal(err)
	}
	pendingRestore = dir
	t.Cleanup(func() { pendingRestore = "" })

	restorePendingDump(testApp(t))

	if pendingRestore != "" {
		t.Errorf("pendingRestore = %q after restorePendingDump, want cleared", pendingRestore)
	}
}
