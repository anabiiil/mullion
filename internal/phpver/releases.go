package phpver

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Release is a downloadable build of PHP for this platform.
type Release struct {
	Version string
	ZipURL  string // Windows: the NTS x64 zip. Elsewhere: the static CLI tarball.
	FpmURL  string // php-fpm tarball (non-Windows only; the Windows zip ships php-cgi).
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// Resolve finds the best release matching the selector: newest match from
// the current branches first, then from the archives for older versions.
func Resolve(ctx context.Context, sel Selector) (Release, error) {
	current, err := FetchCurrent(ctx)
	if err != nil {
		return Release{}, fmt.Errorf("fetching current releases: %w", err)
	}
	if r, ok := newestMatch(current, sel); ok {
		return r, nil
	}
	archived, err := fetchArchives(ctx)
	if err != nil {
		return Release{}, fmt.Errorf("PHP %s is not a current release and the archives lookup failed: %w", sel, err)
	}
	if r, ok := newestMatch(archived, sel); ok {
		return r, nil
	}
	return Release{}, fmt.Errorf("no %s build found for PHP %s", buildLabel, sel)
}

func newestMatch(releases []Release, sel Selector) (Release, bool) {
	best := Release{}
	found := false
	for _, r := range releases {
		if sel.Matches(r.Version) && (!found || Compare(r.Version, best.Version) > 0) {
			best, found = r, true
		}
	}
	return best, found
}
