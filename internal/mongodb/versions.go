package mongodb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// MongoDB publishes its release index in two files of the same shape:
// current.json (~400 KB, the newest release of every supported series)
// and full.json (~50 MB, every release ever). current.json answers
// everything but "install this exact older version", for which
// full.json is streamed — one release object at a time, never loaded
// whole.
const (
	currentIndexURL = "https://downloads.mongodb.org/current.json"
	fullIndexURL    = "https://downloads.mongodb.org/full.json"
)

// minSeries is the oldest release line offered: older ones are long
// past end of life (and have no Apple Silicon builds).
const minSeries = "6.0"

// release is one downloadable server release for this OS/arch.
type release struct {
	Version string
	URL     string
	SHA256  string
	Warning bool // MongoDB flagged it with a "critical issue" warning
}

// indexVersion mirrors the parts of an index entry we use.
type indexVersion struct {
	Version           string `json:"version"`
	ProductionRelease bool   `json:"production_release"`
	UserWarning       string `json:"user_warning"`
	Downloads         []struct {
		Target  string `json:"target"`
		Arch    string `json:"arch"`
		Edition string `json:"edition"`
		Archive struct {
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"archive"`
	} `json:"downloads"`
}

// parseIndex streams a current.json/full.json document and keeps the
// stable community ("base") releases that have an archive for the given
// index target/arch.
func parseIndex(r io.Reader, target, arch string) ([]release, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	var out []release
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if key, _ := tok.(string); key != "versions" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		if err := expectDelim(dec, '['); err != nil {
			return nil, err
		}
		for dec.More() {
			var v indexVersion
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			if !v.ProductionRelease || !fullVersionRe.MatchString(v.Version) {
				continue
			}
			for _, d := range v.Downloads {
				if d.Edition == "base" && d.Target == target && d.Arch == arch && d.Archive.URL != "" {
					out = append(out, release{
						Version: v.Version,
						URL:     d.Archive.URL,
						SHA256:  d.Archive.SHA256,
						Warning: v.UserWarning != "",
					})
					break
				}
			}
		}
		if err := expectDelim(dec, ']'); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("unexpected release index format (wanted %q, got %v)", want, tok)
	}
	return nil
}

// Both indexes are cached for the life of the process.
var (
	indexMu    sync.Mutex
	indexCache = map[string][]release{}
)

func loadIndex(ctx context.Context, url string) ([]release, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	indexMu.Lock()
	defer indexMu.Unlock()
	if rels, ok := indexCache[url]; ok {
		return rels, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the MongoDB release index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	rels, err := parseIndex(resp.Body, indexTarget, indexArch)
	if err != nil {
		return nil, fmt.Errorf("parsing the MongoDB release index: %w", err)
	}
	indexCache[url] = rels
	return rels, nil
}

func seriesOf(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// seriesList returns the offered release lines, newest first.
func seriesList(rels []release) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rels {
		s := seriesOf(r.Version)
		if r.Warning || seen[s] || compare(s+".0", minSeries+".0") < 0 {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return compare(out[i]+".0", out[j]+".0") > 0 })
	return out
}

// selectVersion resolves an install argument against an index:
// "" -> newest DefaultSeries release, "latest" -> newest release of the
// newest offered series, "8.0" -> newest of that series, "8.0.12" ->
// that exact release (even one MongoDB flagged, since it was asked for
// by name). Releases flagged with a critical-issue warning are never
// picked automatically.
func selectVersion(rels []release, arg string) (string, error) {
	arg = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(arg)), "v")
	switch {
	case arg == "":
		return newestIn(rels, DefaultSeries)
	case arg == "latest":
		series := seriesList(rels)
		if len(series) == 0 {
			return "", fmt.Errorf("no stable MongoDB release found for this platform")
		}
		return newestIn(rels, series[0])
	case fullVersionRe.MatchString(arg):
		for _, r := range rels {
			if r.Version == arg {
				return arg, nil
			}
		}
		return "", fmt.Errorf("MongoDB %s is not available for this platform", arg)
	case strings.Count(arg, ".") == 1:
		return newestIn(rels, arg)
	}
	return "", fmt.Errorf("invalid version %q — use a full version (8.0.12), a series (8.0), or `latest`", arg)
}

func newestIn(rels []release, series string) (string, error) {
	best := ""
	for _, r := range rels {
		if r.Warning || seriesOf(r.Version) != series {
			continue
		}
		if best == "" || compare(r.Version, best) > 0 {
			best = r.Version
		}
	}
	if best == "" {
		return "", fmt.Errorf("no stable MongoDB %s release found for this platform", series)
	}
	return best, nil
}

// ResolveVersion turns a CLI/UI version argument ("", "latest", "8.0",
// "8.0.12") into a full stable version downloadable for this OS/arch.
func ResolveVersion(ctx context.Context, arg string) (string, error) {
	if err := platformSupported(); err != nil {
		return "", err
	}
	rels, err := loadIndex(ctx, currentIndexURL)
	if err == nil {
		if v, selErr := selectVersion(rels, arg); selErr == nil {
			return v, nil
		}
	}
	// current.json only has each series' newest release (and none when
	// that one is flagged): consult the full history.
	full, fullErr := loadIndex(ctx, fullIndexURL)
	if fullErr != nil {
		// Offline: an exact version can still be tried as-is.
		if a := strings.TrimPrefix(strings.TrimSpace(arg), "v"); fullVersionRe.MatchString(a) {
			return a, nil
		}
		return "", fullErr
	}
	return selectVersion(full, arg)
}

// AvailableSeries lists the release lines installable on this OS/arch,
// newest first, e.g. ["8.3", "8.2", "8.0", "7.0", "6.0"].
func AvailableSeries(ctx context.Context) ([]string, error) {
	rels, err := loadIndex(ctx, currentIndexURL)
	if err != nil {
		return nil, err
	}
	series := seriesList(rels)
	if len(series) == 0 {
		return nil, fmt.Errorf("no stable MongoDB release found for this platform")
	}
	return series, nil
}

// lookupRelease finds the download for an exact version, first in the
// small index, then in the full one. ok is false when neither knows it.
func lookupRelease(ctx context.Context, version string) (release, bool) {
	for _, url := range []string{currentIndexURL, fullIndexURL} {
		rels, err := loadIndex(ctx, url)
		if err != nil {
			continue
		}
		for _, r := range rels {
			if r.Version == version {
				return r, true
			}
		}
	}
	return release{}, false
}

// ---- mongosh ----

// mongoshIndexURL is MongoDB's own release index for the shell (same
// data as the GitHub releases, without GitHub's API rate limit).
const mongoshIndexURL = "https://downloads.mongodb.com/compass/mongosh.json"

// mongoshFallback is used when neither the index nor GitHub answers.
const mongoshFallback = "2.12.0"

type shellRelease struct {
	Version string
	URL     string
	SHA256  string
}

// parseMongoshIndex picks the newest stable mongosh with a zip for the
// given distro ("darwin", "win32") and index arch ("arm64", "x86_64").
func parseMongoshIndex(r io.Reader, distro, arch string) (shellRelease, error) {
	var index struct {
		Versions []struct {
			Version   string `json:"version"`
			Downloads []struct {
				Arch    string `json:"arch"`
				Distro  string `json:"distro"`
				Archive struct {
					Type   string `json:"type"`
					URL    string `json:"url"`
					SHA256 string `json:"sha256"`
				} `json:"archive"`
			} `json:"downloads"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(r).Decode(&index); err != nil {
		return shellRelease{}, err
	}
	var best shellRelease
	for _, v := range index.Versions {
		if !fullVersionRe.MatchString(v.Version) {
			continue
		}
		for _, d := range v.Downloads {
			if d.Distro == distro && d.Arch == arch && d.Archive.Type == "zip" && d.Archive.URL != "" {
				if best.Version == "" || compare(v.Version, best.Version) > 0 {
					best = shellRelease{Version: v.Version, URL: d.Archive.URL, SHA256: d.Archive.SHA256}
				}
				break
			}
		}
	}
	if best.Version == "" {
		return best, fmt.Errorf("no mongosh build for %s/%s in the index", distro, arch)
	}
	return best, nil
}

// latestMongosh resolves the shell to install: MongoDB's index, then
// GitHub's "latest release" redirect (not the rate-limited API), then a
// pinned version.
func latestMongosh(ctx context.Context) shellRelease {
	client := &http.Client{Timeout: 60 * time.Second}
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, mongoshIndexURL, nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			rel, perr := shellRelease{}, fmt.Errorf("HTTP %s", resp.Status)
			if resp.StatusCode == http.StatusOK {
				rel, perr = parseMongoshIndex(resp.Body, mongoshDistro, indexArch)
			}
			resp.Body.Close()
			if perr == nil {
				return rel
			}
		}
	}
	noRedirect := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"https://github.com/mongodb-js/mongosh/releases/latest", nil); err == nil {
		if resp, err := noRedirect.Do(req); err == nil {
			resp.Body.Close()
			loc := resp.Header.Get("Location")
			if i := strings.LastIndex(loc, "/v"); i >= 0 && fullVersionRe.MatchString(loc[i+2:]) {
				v := loc[i+2:]
				return shellRelease{Version: v, URL: mongoshURL(v)}
			}
		}
	}
	return shellRelease{Version: mongoshFallback, URL: mongoshURL(mongoshFallback)}
}
