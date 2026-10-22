//go:build !windows

package phpver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLatestBuildsStaticListing serves a fake static-php.dev listing:
// only versions with BOTH a cli and an fpm build for this platform
// count, and each branch reports its newest.
func TestLatestBuildsStaticListing(t *testing.T) {
	suffix := platformSuffix()
	var entries []map[string]any
	add := func(name string) { entries = append(entries, map[string]any{"name": name, "is_dir": false}) }
	for _, v := range []string{"8.4.22", "8.4.23"} {
		add("php-" + v + "-cli-" + suffix + ".tar.gz")
		add("php-" + v + "-fpm-" + suffix + ".tar.gz")
	}
	add("php-8.4.27-cli-" + suffix + ".tar.gz")      // no fpm build yet: not installable
	add("php-8.4.30-cli-linux-someotherarch.tar.gz") // another platform
	add("php-8.3.1-cli-" + suffix + ".tar.gz")       // cli only
	add("php-8.3.1-fpm-" + suffix + ".tar.gz")       // ... and fpm: installable
	add("php-8.3.9-micro-" + suffix + ".tar.gz")     // other flavour
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(entries)
	}))
	defer srv.Close()
	old := bulkBase
	bulkBase = srv.URL + "/"
	defer func() { bulkBase = old }()

	got, err := latestBuilds(context.Background(), []string{"8.4", "8.3", "8.2"})
	if err != nil {
		t.Fatal(err)
	}
	if got["8.4"].Version != "8.4.23" || got["8.4"].FpmURL == "" {
		t.Errorf("8.4 = %+v, want 8.4.23 with an fpm build", got["8.4"])
	}
	if got["8.3"].Version != "8.3.1" {
		t.Errorf("8.3 = %+v, want 8.3.1", got["8.3"])
	}
	if _, ok := got["8.2"]; ok {
		t.Errorf("8.2 has no builds but got %+v", got["8.2"])
	}
}
