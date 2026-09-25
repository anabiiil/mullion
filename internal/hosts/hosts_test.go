package hosts

import (
	"strings"
	"testing"
)

const base = `# Copyright (c) 1993-2009 Microsoft Corp.
127.0.0.1 localhost
`

func TestRenderAddsManagedBlock(t *testing.T) {
	out := Render(base, []string{"blog.test", "shop.test"})
	for _, want := range []string{
		"127.0.0.1 localhost",
		beginMarker,
		"127.0.0.1 blog.test",
		"::1 blog.test",
		"127.0.0.1 shop.test",
		endMarker,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderIsIdempotent(t *testing.T) {
	once := Render(base, []string{"blog.test"})
	twice := Render(once, []string{"blog.test"})
	if once != twice {
		t.Fatalf("render is not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestRenderReplacesOldEntries(t *testing.T) {
	old := Render(base, []string{"old.test"})
	out := Render(old, []string{"new.test"})
	if strings.Contains(out, "old.test") {
		t.Fatalf("stale entry survived:\n%s", out)
	}
	if !strings.Contains(out, "new.test") {
		t.Fatalf("new entry missing:\n%s", out)
	}
}

func TestRenderEmptyRemovesBlock(t *testing.T) {
	withSites := Render(base, []string{"blog.test"})
	out := Render(withSites, nil)
	if strings.Contains(out, beginMarker) || strings.Contains(out, "blog.test") {
		t.Fatalf("block not removed:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1 localhost") {
		t.Fatalf("user content lost:\n%s", out)
	}
}

func TestRenderPreservesUserLines(t *testing.T) {
	custom := base + "192.168.1.5 nas.home\n"
	out := Render(custom, []string{"blog.test"})
	if !strings.Contains(out, "192.168.1.5 nas.home") {
		t.Fatalf("user line lost:\n%s", out)
	}
}
