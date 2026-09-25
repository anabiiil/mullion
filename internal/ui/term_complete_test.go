package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A project folder with npm scripts, a src/ dir and a "clear-cache"
// script that used to hijack `clear`.
func complProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"scripts":{"dev":"vite","build":"vite build","clear-cache":"rm -rf .cache"}}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "src", "components"), 0o755)
	os.WriteFile(filepath.Join(dir, "README.md"), nil, 0o644)
	return dir
}

func first(r complResult) string {
	if len(r.Items) == 0 {
		return ""
	}
	return r.Items[0].Insert
}

func TestCompleteCommandRanking(t *testing.T) {
	dir := complProject(t)
	path := os.Getenv("PATH")

	cases := []struct{ in, first string }{
		{"cle", "clear"}, // a built-in beats the "npm run clear-cache" script
		{"pw", "pwd"},
		{"np", "npm"},
		{"l", "ls"}, // shortest common prefix match first
		{"npm run d", "dev"},
		{"cd sr", "src/"},
	}
	for _, c := range cases {
		if got := first(complete(dir, c.in, path)); got != c.first {
			t.Errorf("complete(%q): first = %q, want %q", c.in, got, c.first)
		}
	}
}

func TestCompleteExactMatchNeverHijacksEnter(t *testing.T) {
	dir := complProject(t)
	path := os.Getenv("PATH")
	for _, in := range []string{"clear", "ls", "pwd", "cd", "git"} {
		r := complete(dir, in, path)
		// Either nothing to show, or the exact word is the highlighted
		// (first) item — so the popup leaves Enter to the shell.
		if len(r.Items) > 0 && r.Items[0].Insert != in {
			t.Errorf("complete(%q): first item %q, want the exact word or no popup", in, r.Items[0].Insert)
		}
	}
	// An exact command brings no fuzzy noise ("clear" ~ "calendar").
	for _, it := range complete(dir, "clear", path).Items {
		if it.Insert != "clear" && !strings.HasPrefix(it.Label, "clear") && !strings.Contains(it.Label, " clear") {
			t.Errorf("complete(clear) offers unrelated %q", it.Label)
		}
	}
	// When the exact word is the only match there is no popup at all.
	if r := complete(dir, "npm run dev", path); len(r.Items) != 0 {
		t.Errorf("complete(npm run dev) = %+v, want no items", r.Items)
	}
}

func TestCompleteEmptyCommandWord(t *testing.T) {
	dir := complProject(t)
	for _, in := range []string{"", "  ", "echo hi | ", "ls && "} {
		if r := complete(dir, in, os.Getenv("PATH")); len(r.Items) != 0 {
			t.Errorf("complete(%q) = %d items, want none", in, len(r.Items))
		}
	}
}

func TestPathExecutables(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "mytool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "notexec"), nil, 0o644)
	names := pathExecutables(bin)
	has := map[string]bool{}
	for _, n := range names {
		has[n] = true
	}
	if !has["mytool"] {
		t.Errorf("mytool missing from %v", names)
	}
	if has["notexec"] && os.PathSeparator == '/' {
		t.Errorf("non-executable file listed")
	}
	if got := first(complete(t.TempDir(), "mytoo", bin)); got != "mytool" {
		t.Errorf("PATH executable not offered: first = %q", got)
	}
}
