package pkgsearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetCache clears the shared in-memory cache so tests don't leak
// results between cases.
func resetCache() {
	cacheMu.Lock()
	cache = map[string]cacheEntry{}
	cacheMu.Unlock()
}

func TestSearchComposer(t *testing.T) {
	resetCache()

	searchFixture := `{
		"results": [
			{"name": "laravel/sanctum", "description": "Laravel API auth", "url": "https://packagist.org/packages/laravel/sanctum", "repository": "https://github.com/laravel/sanctum", "downloads": 12345, "favers": 678}
		]
	}`
	p2Fixture := `{
		"packages": {
			"laravel/sanctum": [
				{"version": "dev-master"},
				{"version": "v4.0.2"},
				{"version": "v4.0.1"},
				{"version": "v4.1.0-beta1"}
			]
		}
	}`

	searchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "mullion" {
			t.Errorf("User-Agent = %q, want mullion", got)
		}
		if got := r.URL.Query().Get("q"); got != "sanctum" {
			t.Errorf("q = %q, want sanctum", got)
		}
		w.Write([]byte(searchFixture))
	}))
	defer searchSrv.Close()

	p2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/laravel/sanctum.json") {
			t.Errorf("p2 path = %q, want suffix laravel/sanctum.json", r.URL.Path)
		}
		w.Write([]byte(p2Fixture))
	}))
	defer p2Srv.Close()

	oldSearch, oldP2 := packagistSearchURL, p2BaseURL
	packagistSearchURL, p2BaseURL = searchSrv.URL, p2Srv.URL
	defer func() { packagistSearchURL, p2BaseURL = oldSearch, oldP2 }()

	got, err := SearchComposer(context.Background(), "sanctum", 10)
	if err != nil {
		t.Fatalf("SearchComposer: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("SearchComposer returned %d results, want 1", len(got))
	}
	pkg := got[0]
	if pkg.Name != "laravel/sanctum" {
		t.Errorf("Name = %q", pkg.Name)
	}
	if pkg.Downloads != 12345 || pkg.Stars != 678 {
		t.Errorf("Downloads/Stars = %d/%d, want 12345/678", pkg.Downloads, pkg.Stars)
	}
	// The dev and beta releases must be skipped in favor of the newest
	// stable one.
	if pkg.Version != "v4.0.2" {
		t.Errorf("Version = %q, want v4.0.2", pkg.Version)
	}
}

func TestSearchComposerCaches(t *testing.T) {
	resetCache()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"results": []}`))
	}))
	defer srv.Close()

	oldSearch := packagistSearchURL
	packagistSearchURL = srv.URL
	defer func() { packagistSearchURL = oldSearch }()

	if _, err := SearchComposer(context.Background(), "foo", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := SearchComposer(context.Background(), "foo", 5); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (second should hit cache), got %d", calls)
	}
	// A different query must not be cached under the same key.
	if _, err := SearchComposer(context.Background(), "bar", 5); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected a fresh call for a different query, got %d total calls", calls)
	}
}

func TestSearchComposerEnrichSkipsFailures(t *testing.T) {
	resetCache()
	// Packagist returns two results; the p2 endpoint 404s for both, so
	// the search must still succeed with empty Version fields.
	results := make([]map[string]any, 0, 2)
	for _, name := range []string{"vendor/one", "vendor/two"} {
		results = append(results, map[string]any{"name": name})
	}
	body, _ := json.Marshal(map[string]any{"results": results})

	searchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer searchSrv.Close()
	p2Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer p2Srv.Close()

	oldSearch, oldP2 := packagistSearchURL, p2BaseURL
	packagistSearchURL, p2BaseURL = searchSrv.URL, p2Srv.URL
	defer func() { packagistSearchURL, p2BaseURL = oldSearch, oldP2 }()

	got, err := SearchComposer(context.Background(), "vendor", 10)
	if err != nil {
		t.Fatalf("SearchComposer: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	for _, pkg := range got {
		if pkg.Version != "" {
			t.Errorf("%s: Version = %q, want empty (p2 lookup failed)", pkg.Name, pkg.Version)
		}
	}
}

func TestSearchNpm(t *testing.T) {
	resetCache()
	fixture := `{
		"objects": [
			{
				"package": {
					"name": "dayjs",
					"version": "1.11.13",
					"description": "2KB immutable date library",
					"date": "2024-05-31T00:00:00.000Z",
					"links": {"npm": "https://www.npmjs.com/package/dayjs", "homepage": "https://day.js.org", "repository": "https://github.com/iamkun/dayjs"}
				},
				"score": {"detail": {"popularity": 0.98}}
			}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("text"); got != "dayjs" {
			t.Errorf("text = %q, want dayjs", got)
		}
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	old := npmSearchURL
	npmSearchURL = srv.URL
	defer func() { npmSearchURL = old }()

	got, err := SearchNpm(context.Background(), "dayjs", 10)
	if err != nil {
		t.Fatalf("SearchNpm: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	pkg := got[0]
	if pkg.Name != "dayjs" || pkg.Version != "1.11.13" {
		t.Errorf("Name/Version = %s/%s", pkg.Name, pkg.Version)
	}
	if pkg.URL != "https://www.npmjs.com/package/dayjs" {
		t.Errorf("URL = %q", pkg.URL)
	}
	if pkg.Repository != "https://github.com/iamkun/dayjs" {
		t.Errorf("Repository = %q", pkg.Repository)
	}
	if pkg.Updated == "" {
		t.Error("Updated is empty")
	}
}

func TestSearchNpmCaches(t *testing.T) {
	resetCache()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"objects": []}`))
	}))
	defer srv.Close()

	old := npmSearchURL
	npmSearchURL = srv.URL
	defer func() { npmSearchURL = old }()

	if _, err := SearchNpm(context.Background(), "foo", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := SearchNpm(context.Background(), "foo", 5); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", calls)
	}
}

func TestSearchComposerUpstreamError(t *testing.T) {
	resetCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := packagistSearchURL
	packagistSearchURL = srv.URL
	defer func() { packagistSearchURL = old }()

	if _, err := SearchComposer(context.Background(), "foo", 5); err == nil {
		t.Fatal("expected an error from a failing upstream, got nil")
	}
}

func TestCompareComposerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v4.0.2", "v4.0.1", 1},
		{"1.2.0", "1.10.0", -1},
		{"2.0.0", "2.0.0", 0},
	}
	for _, c := range cases {
		if got := compareComposerVersion(c.a, c.b); (got > 0) != (c.want > 0) || (got < 0) != (c.want < 0) || (got == 0) != (c.want == 0) {
			t.Errorf("compareComposerVersion(%q, %q) = %d, want sign of %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsUnstableComposerVersion(t *testing.T) {
	stable := []string{"1.2.3", "v4.0.2", "10.0.0"}
	unstable := []string{"dev-master", "1.0.0-dev", "v4.1.0-beta1", "2.0.0-alpha1", "3.0.0-RC1"}
	for _, v := range stable {
		if isUnstableComposerVersion(v) {
			t.Errorf("isUnstableComposerVersion(%q) = true, want false", v)
		}
	}
	for _, v := range unstable {
		if !isUnstableComposerVersion(v) {
			t.Errorf("isUnstableComposerVersion(%q) = false, want true", v)
		}
	}
}
