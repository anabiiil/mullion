// Package frankenphp installs and inspects the official FrankenPHP
// release binaries (github.com/php/frankenphp). Each release is a
// self-contained build: the Go server, Caddy and a statically linked
// PHP, so a site that opts into FrankenPHP needs nothing else.
package frankenphp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// ReleasesAPI lists the FrankenPHP releases, newest first. A variable so
// tests can point it at an httptest server.
var ReleasesAPI = "https://api.github.com/repos/php/frankenphp/releases"

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// Asset is one downloadable file of a GitHub release.
type Asset struct {
	Name   string
	URL    string
	Size   int64
	Digest string // "sha256:<hex>", filled in by GitHub
}

// Release is a FrankenPHP release and the one asset this platform uses.
type Release struct {
	Tag   string // "v1.14.0"
	Asset Asset
}

// assetName is the release file for a platform. The project publishes a
// raw static binary for Apple Silicon and a zip for Windows x64; there is
// no Intel Mac build.
func assetName(goos, goarch string) (string, error) {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "frankenphp-mac-arm64", nil
	case goos == "darwin":
		return "", fmt.Errorf("FrankenPHP has no build for Intel Macs — keep this site on PHP-FPM")
	case goos == "windows" && goarch == "amd64":
		return "frankenphp-windows-x86_64.zip", nil
	}
	return "", fmt.Errorf("FrankenPHP builds are not available for %s/%s through Mullion", goos, goarch)
}

type ghRelease struct {
	Tag        string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// Latest is the highest stable release that has a build for this
// platform. A release published before its binaries are attached is
// skipped, not an error.
func Latest(ctx context.Context) (Release, error) {
	return latest(ctx, runtime.GOOS, runtime.GOARCH)
}

func latest(ctx context.Context, goos, goarch string) (Release, error) {
	want, err := assetName(goos, goarch)
	if err != nil {
		return Release{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesAPI, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("checking for FrankenPHP releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("checking for FrankenPHP releases: HTTP %s", resp.Status)
	}
	var list []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return Release{}, fmt.Errorf("reading the FrankenPHP releases: %w", err)
	}
	// GitHub orders by creation date, so a patch release for an older
	// line (v1.13.9 after v1.14.0) can come first: take the highest tag.
	var best Release
	for _, r := range list {
		if r.Prerelease || r.Draft || !validTag(r.Tag) {
			continue
		}
		if best.Tag != "" && !newerTag(r.Tag, best.Tag) {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == want {
				best = Release{Tag: r.Tag, Asset: Asset{Name: a.Name, URL: a.URL, Size: a.Size, Digest: a.Digest}}
				break
			}
		}
	}
	if best.Tag == "" {
		return Release{}, fmt.Errorf("no stable FrankenPHP release has a %s build", want)
	}
	return best, nil
}

// newerTag reports whether tag a is a higher version than b (both valid).
func newerTag(a, b string) bool {
	va, _ := parseTag(a)
	vb, _ := parseTag(b)
	for k := range va {
		if va[k] != vb[k] {
			return va[k] > vb[k]
		}
	}
	return false
}

// validTag accepts only "vX.Y.Z" so a tag can never be a path.
func validTag(tag string) bool {
	_, ok := parseTag(tag)
	return ok
}

// parseTag splits "v1.14.0" into its numbers.
func parseTag(tag string) ([3]int, bool) {
	var v [3]int
	if !strings.HasPrefix(tag, "v") {
		return v, false
	}
	parts := strings.Split(tag[1:], ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return v, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, false
			}
			n = n*10 + int(c-'0')
		}
		v[i] = n
	}
	return v, true
}
