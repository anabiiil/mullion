package mterm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

var sampleAssets = []Asset{
	{Name: "Mullion-Terminal-0.1.5-mac-arm64.dmg"},
	{Name: "Mullion-Terminal-0.1.5-mac-arm64.dmg.blockmap"},
	{Name: "Mullion-Terminal-0.1.5-mac-arm64.zip.blockmap"},
	{Name: "Mullion-Terminal-0.1.5-mac-arm64.zip"},
	{Name: "Mullion-Terminal-0.1.5-win-x64.exe.blockmap"},
	{Name: "Mullion-Terminal-0.1.5-win-x64.exe"},
	{Name: "Mullion-Terminal-0.1.5-linux-amd64.deb"},
	{Name: "latest-mac.yml"},
}

func TestPickAsset(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "Mullion-Terminal-0.1.5-mac-arm64.zip"},
		{"windows", "amd64", "Mullion-Terminal-0.1.5-win-x64.exe"},
		{"darwin", "amd64", ""}, // no Intel Mac build
		{"windows", "arm64", ""},
		{"linux", "amd64", ""},
	}
	for _, c := range cases {
		a, ok := pickAsset(sampleAssets, c.goos, c.goarch)
		if ok != (c.want != "") || a.Name != c.want {
			t.Errorf("%s/%s: got %q (ok=%v), want %q", c.goos, c.goarch, a.Name, ok, c.want)
		}
	}
	if _, ok := pickAsset([]Asset{{Name: "Mullion-Terminal-0.1.5-mac-arm64.dmg"}}, "darwin", "arm64"); ok {
		t.Error("picked a dmg; only the zip is installable")
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.6", "0.1.5", true},
		{"0.1.10", "0.1.9", true},
		{"v0.2.0", "0.1.99", true},
		{"0.1.5", "0.1.5", false},
		{"0.1.5", "0.1.6", false},
		{"1.0", "0.9.9", true},
		{"0.1.5-beta.1", "0.1.5", false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// withAPI points the package at a fake GitHub API for one test.
func withAPI(t *testing.T, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = old })
}

func TestLatest(t *testing.T) {
	withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"v0.1.5","assets":[{"name":"Mullion-Terminal-0.1.5-win-x64.exe","browser_download_url":"http://x/y.exe","size":42}]}`))
	}))
	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version() != "0.1.5" || len(rel.Assets) != 1 || rel.Assets[0].Size != 42 || rel.Assets[0].URL != "http://x/y.exe" {
		t.Errorf("unexpected release %+v", rel)
	}
}

func TestLatestHTTPError(t *testing.T) {
	withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	if _, err := Latest(context.Background()); err == nil {
		t.Error("expected an error for HTTP 403")
	}
}
