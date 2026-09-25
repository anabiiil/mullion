// Package pkgsearch looks up Composer (Packagist) and npm packages for
// the control panel's "add a package" search box. It never installs
// anything — see internal/app/packages.go for that.
package pkgsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Package is one search result, shaped the same regardless of source so
// the UI can render Composer and npm hits with a single component.
type Package struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	Repository  string `json:"repository,omitempty"`
	Downloads   int64  `json:"downloads,omitempty"`
	Stars       int64  `json:"stars,omitempty"`
	Updated     string `json:"updated,omitempty"`
}

// packagistSearchURL and npmSearchURL are vars so tests can point them
// at an httptest server. p2BaseURL likewise for the per-package
// metadata endpoint used to enrich the top Composer results.
var (
	packagistSearchURL = "https://packagist.org/search.json"
	npmSearchURL       = "https://registry.npmjs.org/-/v1/search"
	p2BaseURL          = "https://repo.packagist.org/p2"
)

var httpClient = &http.Client{Timeout: 8 * time.Second}

const userAgent = "mullion"

const cacheTTL = 10 * time.Minute

// enrichLimit caps how many top Composer results get a p2 lookup for
// their latest stable version — enough to be useful, few enough that a
// slow/failing registry can't make a search feel hung.
const enrichLimit = 8

// enrichTimeout bounds each individual p2 lookup so one slow package
// can't stall the others (they run in parallel) or blow past the
// overall search's usefulness window.
const enrichTimeout = 3 * time.Second

type cacheEntry struct {
	packages []Package
	fetched  time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

func cacheKey(source, query string, limit int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", source, query, limit)
}

func cacheGet(key string) ([]Package, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := cache[key]
	if !ok || time.Since(entry.fetched) >= cacheTTL {
		return nil, false
	}
	out := make([]Package, len(entry.packages))
	copy(out, entry.packages)
	return out, true
}

func cacheSet(key string, packages []Package) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	stored := make([]Package, len(packages))
	copy(stored, packages)
	cache[key] = cacheEntry{packages: stored, fetched: time.Now()}
}

func doGet(ctx context.Context, reqURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %s", reqURL, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SearchComposer searches Packagist for packages matching query, and
// enriches the top few results with their latest stable version from
// the p2 metadata endpoint (best-effort — a slow or failing lookup for
// one package never fails the whole search).
func SearchComposer(ctx context.Context, query string, limit int) ([]Package, error) {
	if limit <= 0 {
		limit = 20
	}
	key := cacheKey("composer", query, limit)
	if cached, ok := cacheGet(key); ok {
		return cached, nil
	}

	reqURL := fmt.Sprintf("%s?q=%s&per_page=%d", packagistSearchURL, queryEscape(query), limit)
	var doc struct {
		Results []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Repository  string `json:"repository"`
			Downloads   int64  `json:"downloads"`
			Favers      int64  `json:"favers"`
		} `json:"results"`
	}
	if err := doGet(ctx, reqURL, &doc); err != nil {
		return nil, fmt.Errorf("searching Packagist: %w", err)
	}

	out := make([]Package, len(doc.Results))
	for i, r := range doc.Results {
		out[i] = Package{
			Name:        r.Name,
			Description: r.Description,
			URL:         r.URL,
			Repository:  r.Repository,
			Downloads:   r.Downloads,
			Stars:       r.Favers,
		}
	}

	enrichComposerVersions(ctx, out)

	cacheSet(key, out)
	return out, nil
}

// enrichComposerVersions fills in Version for the first enrichLimit
// entries by querying repo.packagist.org/p2 in parallel. It mutates out
// in place and never returns an error: a failed lookup just leaves that
// entry's Version empty.
func enrichComposerVersions(ctx context.Context, out []Package) {
	n := len(out)
	if n > enrichLimit {
		n = enrichLimit
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := latestStableVersion(ctx, out[i].Name)
			if err != nil {
				return
			}
			out[i].Version = v
		}(i)
	}
	wg.Wait()
}

// latestStableVersion queries repo.packagist.org's p2 metadata for a
// package's newest non-dev, non-prerelease version.
func latestStableVersion(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, enrichTimeout)
	defer cancel()

	reqURL := fmt.Sprintf("%s/%s.json", p2BaseURL, name)
	var doc struct {
		Packages map[string][]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := doGet(ctx, reqURL, &doc); err != nil {
		return "", err
	}
	versions := doc.Packages[name]
	if len(versions) == 0 {
		return "", fmt.Errorf("no versions for %s", name)
	}
	best := ""
	for _, v := range versions {
		if isUnstableComposerVersion(v.Version) {
			continue
		}
		if best == "" || compareComposerVersion(v.Version, best) > 0 {
			best = v.Version
		}
	}
	if best == "" {
		// Nothing stable published — fall back to the newest of
		// whatever exists (p2 lists newest-first, but don't rely on it).
		for _, v := range versions {
			if best == "" || compareComposerVersion(v.Version, best) > 0 {
				best = v.Version
			}
		}
	}
	return best, nil
}

// isUnstableComposerVersion reports whether a Composer version string
// looks like a dev/alpha/beta/RC build rather than a stable release.
func isUnstableComposerVersion(v string) bool {
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "dev-") || strings.HasSuffix(lower, "-dev") {
		return true
	}
	for _, marker := range []string{"alpha", "beta", "rc"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// compareComposerVersion does a best-effort numeric comparison of two
// version strings ("v1.2.3" / "1.2.3"), falling back to a plain string
// comparison when they don't parse as dotted numbers.
func compareComposerVersion(a, b string) int {
	pa := splitVersionParts(a)
	pb := splitVersionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		na, nb := 0, 0
		if i < len(pa) {
			na = pa[i]
		}
		if i < len(pb) {
			nb = pb[i]
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	return strings.Compare(a, b)
}

func splitVersionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n := 0
		matched := true
		for _, c := range f {
			if c < '0' || c > '9' {
				matched = false
				break
			}
			n = n*10 + int(c-'0')
		}
		if !matched {
			break
		}
		out = append(out, n)
	}
	return out
}

// SearchNpm searches the npm registry for packages matching query.
func SearchNpm(ctx context.Context, query string, limit int) ([]Package, error) {
	if limit <= 0 {
		limit = 20
	}
	key := cacheKey("npm", query, limit)
	if cached, ok := cacheGet(key); ok {
		return cached, nil
	}

	reqURL := fmt.Sprintf("%s?text=%s&size=%d", npmSearchURL, queryEscape(query), limit)
	var doc struct {
		Objects []struct {
			Package struct {
				Name        string `json:"name"`
				Version     string `json:"version"`
				Description string `json:"description"`
				Date        string `json:"date"`
				Links       struct {
					NPM        string `json:"npm"`
					Homepage   string `json:"homepage"`
					Repository string `json:"repository"`
				} `json:"links"`
			} `json:"package"`
			Score struct {
				Detail struct {
					Popularity float64 `json:"popularity"`
				} `json:"detail"`
			} `json:"score"`
		} `json:"objects"`
	}
	if err := doGet(ctx, reqURL, &doc); err != nil {
		return nil, fmt.Errorf("searching the npm registry: %w", err)
	}

	out := make([]Package, len(doc.Objects))
	for i, o := range doc.Objects {
		pkgURL := o.Package.Links.NPM
		if pkgURL == "" {
			pkgURL = o.Package.Links.Homepage
		}
		out[i] = Package{
			Name:        o.Package.Name,
			Version:     o.Package.Version,
			Description: o.Package.Description,
			URL:         pkgURL,
			Repository:  o.Package.Links.Repository,
			// npm's popularity score is a 0..1 float, not a raw
			// download count; scale it so it sorts sensibly alongside
			// Packagist's Downloads without claiming false precision.
			Downloads: int64(o.Score.Detail.Popularity * 1_000_000),
			Updated:   o.Package.Date,
		}
	}
	// npm's search already ranks by relevance/popularity; keep that order.

	cacheSet(key, out)
	return out, nil
}

// queryEscape percent-encodes q for use as a single query-string value.
func queryEscape(q string) string {
	return url.QueryEscape(q)
}
