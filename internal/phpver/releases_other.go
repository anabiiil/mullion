//go:build !windows

package phpver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
)

// Off Windows, PHP comes from static-php.dev: fully static, dependency-free
// cli and fpm binaries with a large set of extensions compiled in — the
// same "no dependencies" philosophy as the Windows builds, without Homebrew.
const bulkBase = "https://dl.static-php.dev/static-php-cli/bulk/"

// Source names where installable builds come from (used in CLI output).
const Source = "dl.static-php.dev"

var buildLabel = "static " + platformSuffix() + " (static-php.dev ships PHP 8.0+)"

// platformSuffix is the OS-arch part of static-php.dev's file names,
// e.g. "macos-aarch64" or "linux-x86_64".
func platformSuffix() string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	arch := runtime.GOARCH
	switch arch {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x86_64"
	}
	return osName + "-" + arch
}

// fetchAll lists every version with both a cli and an fpm build for this
// platform, sorted oldest first.
func fetchAll(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bulkBase+"?format=json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s: HTTP %s", bulkBase, resp.Status)
	}
	var entries []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("parsing the static-php.dev listing: %w", err)
	}

	re := regexp.MustCompile(`^php-(\d+\.\d+\.\d+)-(cli|fpm)-` + regexp.QuoteMeta(platformSuffix()) + `\.tar\.gz$`)
	cli, fpm := map[string]string{}, map[string]string{}
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		m := re.FindStringSubmatch(e.Name)
		if m == nil {
			continue
		}
		if m[2] == "cli" {
			cli[m[1]] = e.Name
		} else {
			fpm[m[1]] = e.Name
		}
	}

	var out []Release
	for v, cliName := range cli {
		fpmName, ok := fpm[v]
		if !ok {
			continue
		}
		out = append(out, Release{Version: v, ZipURL: bulkBase + cliName, FpmURL: bulkBase + fpmName})
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

// FetchCurrent returns the newest build of each major.minor branch.
func FetchCurrent(ctx context.Context) ([]Release, error) {
	all, err := fetchAll(ctx)
	if err != nil {
		return nil, err
	}
	newest := map[string]Release{}
	for _, r := range all {
		sel, err := ParseSelector(r.Version)
		if err != nil {
			continue
		}
		branch := fmt.Sprintf("%d.%d", sel.Major, sel.Minor)
		if cur, ok := newest[branch]; !ok || Compare(r.Version, cur.Version) > 0 {
			newest[branch] = r
		}
	}
	out := make([]Release, 0, len(newest))
	for _, r := range newest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

// fetchArchives returns every downloadable version — static-php.dev keeps
// older patch releases in the same directory.
func fetchArchives(ctx context.Context) ([]Release, error) {
	return fetchAll(ctx)
}
