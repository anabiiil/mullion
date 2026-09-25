package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultSeries is what a bare `mullion postgres install` gets.
const DefaultSeries = "17"

// Version discovery uses postgresql.org's own machine-readable release
// list (the same data behind the "Versioning policy" page): one entry
// per major with its latest minor and whether it is still supported.
// EDB — whose binary zips we install — usually publishes the same day
// as the community release, but resolution still confirms the zip
// exists and steps back a minor or two when EDB lags.
var releasesURL = "https://www.postgresql.org/versions.json"

// pinnedLatest is the fallback when versions.json is unreachable: the
// newest release per supported major whose EDB zips were verified to
// exist for both osx and windows-x64 (Sept 2026). To bump: check
// https://www.postgresql.org/versions.json, then HEAD
// https://get.enterprisedb.com/postgresql/postgresql-<v>-1-osx-binaries.zip
// (and -windows-x64-) before editing.
var pinnedLatest = map[string]string{
	"18": "18.6",
	"17": "17.11",
	"16": "16.15",
	"15": "15.19",
	"14": "14.24",
}

// release is one major line from versions.json.
type release struct {
	Major     int
	Latest    int // latest minor
	Supported bool
}

// parseReleases decodes versions.json, keeping the majors that use the
// modern two-part numbering (10+) — nothing older has EDB zips worth
// installing.
func parseReleases(body []byte) ([]release, error) {
	var raw []struct {
		Major       string `json:"major"`
		LatestMinor string `json:"latestMinor"`
		Supported   bool   `json:"supported"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parsing PostgreSQL releases: %w", err)
	}
	var out []release
	for _, r := range raw {
		major, err := strconv.Atoi(r.Major)
		if err != nil || major < 10 {
			continue
		}
		minor, err := strconv.Atoi(r.LatestMinor)
		if err != nil {
			continue
		}
		out = append(out, release{Major: major, Latest: minor, Supported: r.Supported})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no PostgreSQL releases found in the release list")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Major > out[j].Major })
	return out, nil
}

// fetchReleases downloads and parses versions.json (a var for tests).
var fetchReleases = func(ctx context.Context) ([]release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %s", releasesURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseReleases(body)
}

// edbBuilds are the EDB package build numbers tried, newest first
// (EDB re-issues a zip as -2, -3 when it fixes packaging).
var edbBuilds = []int{3, 2, 1}

// downloadURLs returns the candidate zips for a version on this OS.
func downloadURLs(version string) []string {
	if edbPlatform == "" {
		return nil
	}
	var urls []string
	for _, b := range edbBuilds {
		urls = append(urls, fmt.Sprintf(
			"https://get.enterprisedb.com/postgresql/postgresql-%s-%d-%s-binaries.zip", version, b, edbPlatform))
	}
	return urls
}

// available reports whether EDB has a zip of this version for this OS
// (a var for tests). EDB's bucket answers 403 for missing files.
var available = func(ctx context.Context, version string) (bool, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	var lastErr error
	for _, url := range downloadURLs(version) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return false, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true, nil
		}
	}
	return false, lastErr
}

var (
	seriesRe  = regexp.MustCompile(`^\d{2,}$`)
	versionRe = regexp.MustCompile(`^(\d{2,})\.(\d+)$`)
)

// ResolveVersion turns a CLI/UI version argument into a full version
// installable on this OS: "" -> newest of DefaultSeries, "latest" ->
// newest release of the newest major, "16" -> newest 16.x, full
// versions ("17.6") pass through.
func ResolveVersion(ctx context.Context, arg string) (string, error) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	if err := platformSupported(); err != nil {
		return "", err
	}
	switch {
	case arg == "":
		return resolveSeries(ctx, DefaultSeries)
	case arg == "latest":
		series, err := AvailableSeries(ctx)
		if err != nil {
			return "", err
		}
		return resolveSeries(ctx, series[0])
	case seriesRe.MatchString(arg):
		return resolveSeries(ctx, arg)
	case versionRe.MatchString(arg):
		return arg, nil
	}
	return "", fmt.Errorf("invalid PostgreSQL version %q — use a major (17), a full version (17.6), or `latest`", arg)
}

// resolveSeries finds the newest release of one major that EDB actually
// has a zip for, stepping back up to two minors when EDB lags behind
// the community release.
func resolveSeries(ctx context.Context, series string) (string, error) {
	major, _ := strconv.Atoi(series)
	rels, err := fetchReleases(ctx)
	if err != nil {
		if v, ok := pinnedLatest[series]; ok {
			return v, nil
		}
		return "", fmt.Errorf("could not fetch the PostgreSQL release list (%v); pass a full version explicitly, e.g. %s.0", err, series)
	}
	var rel *release
	for i := range rels {
		if rels[i].Major == major {
			rel = &rels[i]
		}
	}
	if rel == nil {
		return "", fmt.Errorf("PostgreSQL %s is not a released major version", series)
	}
	var lastErr error
	for minor := rel.Latest; minor >= 0 && minor >= rel.Latest-2; minor-- {
		v := fmt.Sprintf("%d.%d", major, minor)
		ok, err := available(ctx, v)
		if ok {
			return v, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("checking PostgreSQL %s downloads: %w", series, lastErr)
	}
	return "", fmt.Errorf("no downloadable PostgreSQL %s build found for this platform", series)
}

// AvailableSeries lists the supported majors, newest first — e.g.
// ["18","17","16","15","14"] — for the UI's version picker.
func AvailableSeries(ctx context.Context) ([]string, error) {
	var out []string
	if rels, err := fetchReleases(ctx); err == nil {
		for _, r := range rels {
			if r.Supported {
				out = append(out, strconv.Itoa(r.Major))
			}
		}
	}
	if len(out) == 0 {
		for s := range pinnedLatest {
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool { return compare(out[i], out[j]) > 0 })
	}
	return out, nil
}

// Major returns the major part of a version: "17.6" -> "17".
func Major(version string) string {
	if i := strings.Index(version, "."); i >= 0 {
		return version[:i]
	}
	return version
}

// compare orders versions ("17.6", "17", "9.6.24") numerically.
func compare(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}
