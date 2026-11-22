//go:build !windows

package devserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/pmdir"
)

// A yarn project whose yarn is broken (corepack can't fetch it) still
// starts: its dependencies are installed, so npm runs the script.
func TestBrokenYarnFallsBackToNpm(t *testing.T) {
	home := t.TempDir()
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	nodeDir := t.TempDir()
	bin := filepath.Join(nodeDir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "yarn"), []byte("#!/bin/sh\necho 'corepack: download failed' >&2\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\necho \"npm $*\"\nexec python3 -m http.server \"$PORT\" --bind 127.0.0.1\n"), 0o755)

	project := t.TempDir()
	os.WriteFile(filepath.Join(project, "package.json"), []byte(`{"scripts":{"dev":"vite"}}`), 0o644)
	os.WriteFile(filepath.Join(project, "yarn.lock"), nil, 0o644)
	os.MkdirAll(filepath.Join(project, "node_modules", ".bin"), 0o755)

	site := config.Site{Name: "fb", Path: project, Kind: "node", DevPort: 42977}
	port, err := Ensure(paths, site, "fb.test", nodeDir)
	t.Cleanup(func() { Stop(paths, "fb") })
	if err != nil {
		t.Fatal(err)
	}
	if port != 42977 {
		t.Errorf("port = %d", port)
	}
	log, _ := os.ReadFile(logFilePath(paths, "fb"))
	for _, want := range []string{"corepack: download failed", "running the script with npm instead", "npm run dev"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}

	// Yarn PnP has no node_modules: no fallback, and the reason lands in the log.
	Stop(paths, "fb")
	os.RemoveAll(filepath.Join(project, "node_modules"))
	os.WriteFile(filepath.Join(project, ".pnp.cjs"), nil, 0o644)
	if _, err := Ensure(paths, site, "fb.test", nodeDir); err == nil {
		t.Fatal("broken yarn on a PnP project should fail")
	}
	log, _ = os.ReadFile(logFilePath(paths, "fb"))
	if !strings.Contains(string(log), "mullion: yarn install failed") {
		t.Errorf("failure reason not in the log:\n%s", log)
	}
}
