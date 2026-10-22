package phpver

// Patch updates for installed versions: for every installed version, the
// newest build of the same major.minor that this platform's source
// offers (static-php.dev off Windows, windows.php.net's NTS x64 zips on
// Windows — the same listing Resolve installs from, so a reported update
// is always installable). php.net's releases feed adds whether any patch
// in between was a security release.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"pm/internal/pmdir"
)

// Update is a newer patch release of an installed version's branch.
type Update struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Security reports that php.net tagged at least one release after
	// From (up to To) as a security release. false also means "unknown"
	// (php.net unreachable).
	Security bool `json:"security"`
}

// UpdatesCacheTTL bounds how often Updates really asks the release
// listing (the panel checks on load, `php list` on every run).
const UpdatesCacheTTL = 6 * time.Hour

// updatesTimeout keeps an update check short: it is informational, and
// must never make `php list` or the panel hang when offline.
const updatesTimeout = 10 * time.Second

// securityFeedURL is php.net's releases JSON for one branch ("8.4"),
// newest first; each release carries "tags": ["security"] when it was a
// security release. A var so tests can point it at a fake feed.
var securityFeedURL = func(branch string) string {
	return "https://www.php.net/releases/index.php?json&max=100&version=" + branch
}

// latestBuildsFn finds the newest installable build of each requested
// branch — a seam for tests.
var latestBuildsFn = latestBuilds

// updatesNow is the clock, swappable in tests of the cache.
var updatesNow = time.Now

// Branch is a version's "major.minor" ("8.4.23" -> "8.4"), or "" when
// it isn't a valid version.
func Branch(version string) string {
	sel, err := ParseSelector(version)
	if err != nil || sel.Minor == -1 {
		return ""
	}
	return fmt.Sprintf("%d.%d", sel.Major, sel.Minor)
}

// releaseCache is what Updates remembers between runs (in the php dir,
// next to the fpm pool configs — Installed ignores non-version names).
type releaseCache struct {
	CheckedAt time.Time          `json:"checkedAt"`
	Latest    map[string]Release `json:"latest"`  // branch -> newest build
	Checked   []string           `json:"checked"` // branches looked up (with or without a build)
	// Security lists, per branch, the versions php.net tags as security
	// releases (absent = not fetched yet, or php.net unreachable).
	Security map[string][]string `json:"security,omitempty"`
}

var updatesMu sync.Mutex

func releaseCachePath(paths pmdir.Paths) string {
	return filepath.Join(paths.PhpDir(), "releases-cache.json")
}

func readReleaseCache(paths pmdir.Paths) (releaseCache, bool) {
	var c releaseCache
	data, err := os.ReadFile(releaseCachePath(paths))
	if err != nil || json.Unmarshal(data, &c) != nil || c.Latest == nil {
		return releaseCache{}, false
	}
	return c, true
}

func writeReleaseCache(paths pmdir.Paths, c releaseCache) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(paths.PhpDir(), 0o755)
	_ = os.WriteFile(releaseCachePath(paths), data, 0o644)
}

// covers reports whether a cache entry answers every branch in branches.
func (c releaseCache) covers(branches []string) bool {
	for _, b := range branches {
		if !slices.Contains(c.Checked, b) {
			return false
		}
	}
	return true
}

// UpdatesCheckedAt is when the cached release listing was last fetched
// (zero when there is none).
func UpdatesCheckedAt(paths pmdir.Paths) time.Time {
	c, _ := readReleaseCache(paths)
	return c.CheckedAt
}

// Updates reports, keyed by installed version, the newest build of the
// same major.minor when it is newer than that version. The listing is
// cached for UpdatesCacheTTL (force skips the cache). Offline-safe: an
// unreachable listing returns an error — and the last cached answer
// when there is one — never a hang (requests are bounded by a short
// timeout).
func Updates(ctx context.Context, paths pmdir.Paths, installed []string, force bool) (map[string]Update, error) {
	updatesMu.Lock()
	defer updatesMu.Unlock()

	var branches []string
	for _, v := range installed {
		if b := Branch(v); b != "" && !slices.Contains(branches, b) {
			branches = append(branches, b)
		}
	}
	sort.Strings(branches)
	if len(branches) == 0 {
		return map[string]Update{}, nil
	}

	cache, haveCache := readReleaseCache(paths)
	fresh := haveCache && updatesNow().Sub(cache.CheckedAt) < UpdatesCacheTTL && cache.covers(branches)
	var fetchErr error
	if force || !fresh {
		ctx2, cancel := context.WithTimeout(ctx, updatesTimeout)
		latest, err := latestBuildsFn(ctx2, branches)
		cancel()
		if err != nil {
			fetchErr = fmt.Errorf("checking %s for PHP updates: %w", Source, err)
			if !haveCache {
				return nil, fetchErr
			}
		} else {
			cache = releaseCache{CheckedAt: updatesNow(), Latest: latest, Checked: branches, Security: map[string][]string{}}
		}
	}

	out := map[string]Update{}
	dirty := fetchErr == nil && (force || !fresh)
	for _, v := range installed {
		rel, ok := cache.Latest[Branch(v)]
		if !ok || Compare(rel.Version, v) <= 0 {
			continue
		}
		b := Branch(v)
		if _, known := cache.Security[b]; !known && fetchErr == nil {
			ctx2, cancel := context.WithTimeout(ctx, updatesTimeout)
			if sec, err := fetchSecurityReleases(ctx2, b); err == nil {
				if cache.Security == nil {
					cache.Security = map[string][]string{}
				}
				cache.Security[b] = sec
				dirty = true
			}
			cancel()
		}
		out[v] = Update{From: v, To: rel.Version, Security: securityBetween(cache.Security[b], v, rel.Version)}
	}
	if dirty {
		writeReleaseCache(paths, cache)
	}
	return out, fetchErr
}

// latestBuilds returns the newest build of each requested branch: from
// the current releases first, then (only for branches still missing —
// end-of-life ones) the archives.
func latestBuilds(ctx context.Context, branches []string) (map[string]Release, error) {
	current, err := FetchCurrent(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]Release{}
	add := func(rels []Release) {
		for _, r := range rels {
			b := Branch(r.Version)
			if !slices.Contains(branches, b) {
				continue
			}
			if cur, ok := out[b]; !ok || Compare(r.Version, cur.Version) > 0 {
				out[b] = r
			}
		}
	}
	add(current)
	for _, b := range branches {
		if _, ok := out[b]; !ok {
			// Best effort: an archives hiccup only hides EOL updates.
			if archived, err := fetchArchives(ctx); err == nil {
				add(archived)
			}
			break
		}
	}
	return out, nil
}

// fetchSecurityReleases lists the versions of one branch that php.net
// tags as security releases.
func fetchSecurityReleases(ctx context.Context, branch string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, securityFeedURL(branch), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("php.net releases: HTTP %s", resp.Status)
	}
	return parseSecurityFeed(resp.Body)
}

// parseSecurityFeed reads php.net's {"8.4.27": {"tags": ["security"],
// ...}, ...} map. A single-release answer (no max) has the fields at the
// top level instead; both shapes are accepted.
func parseSecurityFeed(r io.Reader) ([]string, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing php.net's releases feed: %w", err)
	}
	type rel struct {
		Version string   `json:"version"`
		Tags    []string `json:"tags"`
	}
	isSec := func(tags []string) bool {
		for _, t := range tags {
			if strings.EqualFold(strings.TrimSpace(t), "security") {
				return true
			}
		}
		return false
	}
	out := []string{}
	if v, ok := raw["version"]; ok {
		var one rel
		_ = json.Unmarshal(v, &one.Version)
		if t, ok := raw["tags"]; ok {
			_ = json.Unmarshal(t, &one.Tags)
		}
		if one.Version != "" && isSec(one.Tags) {
			out = append(out, one.Version)
		}
		return out, nil
	}
	for ver, msg := range raw {
		var r rel
		if json.Unmarshal(msg, &r) != nil {
			continue
		}
		if _, err := ParseSelector(ver); err == nil && isSec(r.Tags) {
			out = append(out, ver)
		}
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i], out[j]) < 0 })
	return out, nil
}

// securityBetween reports whether any version in secs lies in (from, to].
func securityBetween(secs []string, from, to string) bool {
	for _, s := range secs {
		if Compare(s, from) > 0 && Compare(s, to) <= 0 {
			return true
		}
	}
	return false
}
