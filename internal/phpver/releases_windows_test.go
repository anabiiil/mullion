//go:build windows

package phpver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLatestBuildsWindowsListing serves a fake windows.php.net: the
// current releases.json (NTS x64 picked, never TS or x86) plus the
// archives listing for an end-of-life branch.
func TestLatestBuildsWindowsListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/releases.json":
			fmt.Fprint(w, `{"8.4":{"version":"8.4.27",
				"ts-vs17-x64":{"zip":{"path":"php-8.4.27-Win32-vs17-x64.zip"}},
				"nts-vs17-x86":{"zip":{"path":"php-8.4.27-nts-Win32-vs17-x86.zip"}},
				"nts-vs17-x64":{"zip":{"path":"php-8.4.27-nts-Win32-vs17-x64.zip"}}}}`)
		case "/archives/":
			fmt.Fprint(w, `<a href="php-8.0.29-nts-Win32-vs16-x64.zip">x</a> <a href="php-8.0.30-nts-Win32-vs16-x64.zip">x</a> <a href="php-8.0.31-Win32-vs16-x64.zip">ts</a>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldR, oldA := releasesBase, archivesBase
	releasesBase, archivesBase = srv.URL+"/releases/", srv.URL+"/archives/"
	defer func() { releasesBase, archivesBase = oldR, oldA }()

	got, err := latestBuilds(context.Background(), []string{"8.4", "8.0"})
	if err != nil {
		t.Fatal(err)
	}
	if r := got["8.4"]; r.Version != "8.4.27" || r.ZipURL != srv.URL+"/releases/php-8.4.27-nts-Win32-vs17-x64.zip" {
		t.Errorf("8.4 = %+v, want the NTS x64 8.4.27 zip", r)
	}
	if r := got["8.0"]; r.Version != "8.0.30" {
		t.Errorf("8.0 = %+v, want 8.0.30 (newest NTS x64 in the archives)", r)
	}
}
