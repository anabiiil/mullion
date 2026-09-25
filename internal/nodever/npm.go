package nodever

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// npmRegistryURL is the registry endpoint for npm's own package metadata.
// A var so tests can point it at an httptest server.
var npmRegistryURL = "https://registry.npmjs.org/npm"

var npmVersionsCache struct {
	sync.Mutex
	versions []string
	fetched  time.Time
}

const npmVersionsCacheTTL = 10 * time.Minute

// NpmVersions lists npm's stable (non-prerelease) published versions,
// newest first. The result is cached in memory for 10 minutes.
func NpmVersions(ctx context.Context) ([]string, error) {
	npmVersionsCache.Lock()
	if npmVersionsCache.versions != nil && time.Since(npmVersionsCache.fetched) < npmVersionsCacheTTL {
		out := append([]string(nil), npmVersionsCache.versions...)
		npmVersionsCache.Unlock()
		return out, nil
	}
	npmVersionsCache.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, npmRegistryURL, nil)
	if err != nil {
		return nil, err
	}
	// The abbreviated metadata format skips changelog/readme fields
	// present in the full document, which for a package with npm's
	// history is a large saving.
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("npm registry: HTTP %s", resp.Status)
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing the npm registry response: %w", err)
	}

	var out []string
	for v := range doc.Versions {
		if isPrerelease(v) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return compareSemver(out[i], out[j]) > 0 })

	npmVersionsCache.Lock()
	npmVersionsCache.versions = out
	npmVersionsCache.fetched = time.Now()
	result := append([]string(nil), out...)
	npmVersionsCache.Unlock()
	return result, nil
}

// isPrerelease reports whether a semver string has a "-" prerelease
// component (e.g. "10.0.0-rc.1").
func isPrerelease(v string) bool {
	return strings.Contains(v, "-")
}

// compareSemver orders two plain "X.Y.Z" version strings. Non-numeric
// or missing components sort as 0, so malformed entries don't panic —
// they just sort low.
func compareSemver(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		na, nb := 0, 0
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// NpmVersionOf reads the npm version bundled inside an installed Node
// version directory, or "" if it can't be determined.
func NpmVersionOf(nodeDir string) string {
	path := npmPackageJSON(nodeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Version
}

// npmPackageJSON locates npm's package.json inside a Node version
// directory, matching the platform layout used by BinDir/Tool (see
// bins_other.go / bins_windows.go: unix ships bin/ + lib/node_modules,
// the Windows zip is flat with node_modules at the top level).
func npmPackageJSON(nodeDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(nodeDir, "node_modules", "npm", "package.json")
	}
	return filepath.Join(nodeDir, "lib", "node_modules", "npm", "package.json")
}
