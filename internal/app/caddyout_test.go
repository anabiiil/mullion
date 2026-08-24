//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/pmdir"
)

func TestWriteCaddyfileModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths := pmdir.Paths{Home: filepath.Join(home, ".mullion")}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(home, "proj")
	os.MkdirAll(filepath.Join(proj, "dist"), 0o755)
	os.WriteFile(filepath.Join(proj, "dist", "index.html"), []byte("hi"), 0o644)

	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Paths: paths, State: state}
	a.State.Sites = []config.Site{
		{Name: "b", Path: proj, Kind: "node", Mode: "build", BuildDir: "dist", DevPort: 42001},
		{Name: "p", Path: proj, Kind: "node", DevPaused: true, DevPort: 42002},
		{Name: "r", Path: proj, Kind: "node", DevPort: 42003},
	}
	if err := a.WriteCaddyfile(map[string]int{"r": 5173}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(paths.Caddyfile())
	out := string(data)
	t.Logf("caddyfile:\n%s", out)

	bSection := section(out, "http://b.test")
	if !strings.Contains(bSection, "try_files") || strings.Contains(bSection, "paused") {
		t.Fatalf("build site wrong:\n%s", bSection)
	}
	pSection := section(out, "http://p.test")
	if !strings.Contains(pSection, "paused") {
		t.Fatalf("paused site wrong:\n%s", pSection)
	}
	rSection := section(out, "http://r.test")
	if !strings.Contains(rSection, "reverse_proxy localhost:5173") {
		t.Fatalf("running site wrong:\n%s", rSection)
	}
}

func section(out, host string) string {
	i := strings.Index(out, host)
	if i < 0 {
		return "<missing " + host + ">"
	}
	j := strings.Index(out[i:], "\n}")
	return out[i : i+j]
}
