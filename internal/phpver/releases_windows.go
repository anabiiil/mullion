//go:build windows

package phpver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Source names where installable builds come from (used in CLI output).
const Source = "windows.php.net"

const buildLabel = "Windows x64"

const (
	releasesBase = "https://windows.php.net/downloads/releases/"
	archivesBase = "https://windows.php.net/downloads/releases/archives/"
)

// FetchCurrent returns the actively supported releases (one per branch)
// from windows.php.net's releases.json.
func FetchCurrent(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesBase+"releases.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases.json: HTTP %s", resp.Status)
	}

	// Branch key ("8.3") -> arbitrary keys; builds like "nts-vs16-x64" hold {"zip": {"path": ...}}.
	var raw map[string]map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	var out []Release
	for _, branch := range raw {
		var version string
		if v, ok := branch["version"]; ok {
			_ = json.Unmarshal(v, &version)
		}
		if version == "" {
			continue
		}
		for key, val := range branch {
			if !strings.HasPrefix(key, "nts-") || !strings.HasSuffix(key, "-x64") {
				continue
			}
			var build struct {
				Zip struct {
					Path string `json:"path"`
				} `json:"zip"`
			}
			if err := json.Unmarshal(val, &build); err != nil || build.Zip.Path == "" {
				continue
			}
			out = append(out, Release{Version: version, ZipURL: releasesBase + build.Zip.Path})
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i].Version, out[j].Version) < 0 })
	return out, nil
}

var archiveZipRe = regexp.MustCompile(`php-(\d+\.\d+\.\d+)-nts-Win32-(?:VC|vc|vs)\d+-x64\.zip`)

// fetchArchives scans the archives directory listing for NTS x64 zips of
// end-of-life versions.
func fetchArchives(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archivesBase, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("archives listing: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}

	seen := map[string]Release{}
	for _, m := range archiveZipRe.FindAllStringSubmatch(string(body), -1) {
		zipName, version := m[0], m[1]
		// Keep one build per version; any VS toolchain works for our use.
		if _, ok := seen[version]; !ok {
			seen[version] = Release{Version: version, ZipURL: archivesBase + zipName}
		}
	}
	out := make([]Release, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i].Version, out[j].Version) < 0 })
	return out, nil
}
