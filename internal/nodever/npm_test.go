package nodever

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNpmVersions(t *testing.T) {
	fixture := `{
		"name": "npm",
		"dist-tags": {"latest": "10.9.2"},
		"versions": {
			"10.9.2": {"version": "10.9.2"},
			"10.9.1": {"version": "10.9.1"},
			"11.0.0-pre.0": {"version": "11.0.0-pre.0"},
			"9.8.1": {"version": "9.8.1"},
			"10.0.0-beta.3": {"version": "10.0.0-beta.3"}
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/vnd.npm.install-v1+json" {
			t.Errorf("Accept header = %q, want the abbreviated-metadata value", got)
		}
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	old := npmRegistryURL
	npmRegistryURL = srv.URL
	defer func() { npmRegistryURL = old }()
	npmVersionsCache.versions = nil // reset the process-lifetime cache

	got, err := NpmVersions(context.Background())
	if err != nil {
		t.Fatalf("NpmVersions: %v", err)
	}
	want := []string{"10.9.2", "10.9.1", "9.8.1"}
	if len(got) != len(want) {
		t.Fatalf("NpmVersions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NpmVersions = %v, want %v", got, want)
		}
	}
}

func TestNpmVersionsCached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"versions": {"10.0.0": {"version": "10.0.0"}}}`))
	}))
	defer srv.Close()

	old := npmRegistryURL
	npmRegistryURL = srv.URL
	defer func() { npmRegistryURL = old }()
	npmVersionsCache.versions = nil

	if _, err := NpmVersions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := NpmVersions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected the registry to be fetched once (cached after), got %d calls", calls)
	}
}

func TestNpmVersionOf(t *testing.T) {
	dir := t.TempDir()

	if got := NpmVersionOf(dir); got != "" {
		t.Fatalf("NpmVersionOf on an empty dir = %q, want \"\"", got)
	}

	rel := []string{"lib", "node_modules", "npm"}
	if runtime.GOOS == "windows" {
		rel = []string{"node_modules", "npm"}
	}
	npmDir := filepath.Join(append([]string{dir}, rel...)...)
	if err := os.MkdirAll(npmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"name": "npm", "version": "10.9.2"})
	if err := os.WriteFile(filepath.Join(npmDir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := NpmVersionOf(dir); got != "10.9.2" {
		t.Fatalf("NpmVersionOf = %q, want %q", got, "10.9.2")
	}
}
