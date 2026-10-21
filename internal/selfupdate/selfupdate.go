// Package selfupdate checks GitHub for a newer Mullion release and
// upgrades the running install through the channel it came from:
// Homebrew (`brew upgrade anabiiil/tap/mullion`), Scoop (`scoop update
// mullion`), or a direct download that swaps the executable in place.
//
// An upgrade has two halves, run by two different processes. Upgrade
// runs in the OLD process (the CLI or the control panel that the user
// clicked) and only puts the new binary on disk. The hand-off — making
// the background agent, the Windows tray and the panel window run the
// new code — must happen in the NEW binary, so callers start
// `<new> upgrade --finish`, which calls Relaunch (see relaunch.go).
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"pm/internal/version"
)

// Overridable for tests: where the GitHub API lives and which repo
// publishes Mullion's releases.
var (
	APIBase = "https://api.github.com"
	Repo    = "anabiiil/mullion"
)

// BrewFormula is the formula's fully qualified name. The tap repository
// is anabiiil/homebrew-tap, which Homebrew names "anabiiil/tap".
const BrewFormula = "anabiiil/tap/mullion"

// cacheTTL bounds how often Latest really asks GitHub: the panel checks
// whenever Settings opens, and the unauthenticated API allows only 60
// requests an hour.
const cacheTTL = 30 * time.Minute

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// now is the clock, swappable in tests of the cache.
var now = time.Now

// Asset is one downloadable file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
	// Digest is GitHub's "sha256:<hex>" of the file, when it has one.
	Digest string `json:"digest"`
}

// Release is the subset of GitHub's release JSON Mullion needs.
type Release struct {
	Tag       string    `json:"tag_name"`
	Name      string    `json:"name"`
	URL       string    `json:"html_url"`
	Body      string    `json:"body"`
	Published time.Time `json:"published_at"`
	Assets    []Asset   `json:"assets"`
}

// Version is the release tag without its leading "v".
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

var cache struct {
	sync.Mutex
	rel Release
	at  time.Time
}

// Latest returns the newest published release, answering from a
// 30-minute cache when it can.
func Latest(ctx context.Context) (Release, error) {
	cache.Lock()
	if cache.rel.Tag != "" && now().Sub(cache.at) < cacheTTL {
		rel := cache.rel
		cache.Unlock()
		return rel, nil
	}
	cache.Unlock()
	return LatestFresh(ctx)
}

// LatestFresh always asks GitHub (and refreshes the cache).
func LatestFresh(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mullion/"+version.Number)
	resp, err := httpClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("could not reach GitHub to check for updates: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return Release{}, errors.New("GitHub is rate-limiting update checks right now — try again in an hour")
	case resp.StatusCode != http.StatusOK:
		return Release{}, fmt.Errorf("checking for Mullion releases: HTTP %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("reading the Mullion release: %w", err)
	}
	if rel.Tag == "" {
		return Release{}, errors.New("the latest Mullion release has no version tag")
	}
	cache.Lock()
	cache.rel, cache.at = rel, now()
	cache.Unlock()
	return rel, nil
}

// resetCache forgets the cached release (tests).
func resetCache() {
	cache.Lock()
	cache.rel, cache.at = Release{}, time.Time{}
	cache.Unlock()
}

// Newer reports whether version a is newer than b. Dotted numeric parts
// compare as numbers ("2.1.10" > "2.1.9"); with equal numbers a release
// beats a pre-release of it ("2.2.0" > "2.2.0-rc.1").
func Newer(a, b string) bool {
	na, pa := splitVersion(a)
	nb, pb := splitVersion(b)
	for i := 0; i < len(na) || i < len(nb); i++ {
		var x, y int
		if i < len(na) {
			x = na[i]
		}
		if i < len(nb) {
			y = nb[i]
		}
		if x != y {
			return x > y
		}
	}
	if pa == "" || pb == "" {
		return pa == "" && pb != ""
	}
	return pa > pb
}

func splitVersion(v string) (nums []int, pre string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i] // build metadata never orders
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s)
		nums = append(nums, n)
	}
	return nums, pre
}

// Channel is how this copy of Mullion was installed — which decides how
// it upgrades.
type Channel string

const (
	Homebrew Channel = "homebrew"
	Scoop    Channel = "scoop"
	Direct   Channel = "direct"
)

// Label is the channel's name for people.
func (c Channel) Label() string {
	switch c {
	case Homebrew:
		return "Homebrew"
	case Scoop:
		return "Scoop"
	}
	return "Direct download"
}

// ManualCommand is what to run in a terminal to upgrade by hand.
func (c Channel) ManualCommand() string {
	switch c {
	case Homebrew:
		return "brew update && brew upgrade " + BrewFormula
	case Scoop:
		return "scoop update; scoop update mullion"
	}
	if runtime.GOOS == "windows" {
		return "download mullion.exe from https://github.com/" + Repo + "/releases/latest and run it"
	}
	return "curl -fsSL https://raw.githubusercontent.com/" + Repo + "/main/install.sh | sh"
}

// DetectChannel works out this install's channel from the running
// executable (symlinks resolved).
func DetectChannel() Channel {
	exe, err := os.Executable()
	if err != nil {
		return Direct
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	_, brewHas := brewInstalledMullion()
	_, scoopHas := scoopInstalledMullion()
	return channelFor(exe, brewHas, scoopHas)
}

// channelFor is DetectChannel's decision. A copy that isn't inside a
// package manager's tree (~/.mullion/bin/mullion.exe, which Windows
// installs always copy to) still belongs to the package manager when
// one has Mullion installed: upgrading the copy alone would leave the
// package manager's older binary to downgrade it again on its next run.
func channelFor(exe string, brewInstalled, scoopInstalled bool) Channel {
	p := strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	switch {
	case strings.Contains(p, "/cellar/mullion/"):
		return Homebrew
	case strings.Contains(p, "/apps/mullion/"):
		return Scoop
	case brewInstalled:
		return Homebrew
	case scoopInstalled:
		return Scoop
	}
	return Direct
}

// brewPrefixes are the Homebrew prefixes Mullion knows (Apple silicon,
// Intel, Linuxbrew). Tests point it at a temp dir.
var brewPrefixes = []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}

// brewInstalledMullion finds a Homebrew-installed mullion's Cellar dir.
func brewInstalledMullion() (string, bool) {
	if runtime.GOOS == "windows" {
		return "", false
	}
	for _, p := range brewPrefixes {
		dir := filepath.Join(p, "Cellar", "mullion")
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, true
		}
	}
	return "", false
}

// scoopRoots lists where Scoop may live: $SCOOP, ~/scoop, and the
// global install dir.
var scoopRoots = func() []string {
	var roots []string
	if v := os.Getenv("SCOOP"); v != "" {
		roots = append(roots, v)
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "scoop"))
	}
	if v := os.Getenv("SCOOP_GLOBAL"); v != "" {
		roots = append(roots, v)
	}
	if v := os.Getenv("ProgramData"); v != "" {
		roots = append(roots, filepath.Join(v, "scoop"))
	}
	return roots
}

// scoopInstalledMullion finds Scoop's current mullion.exe.
func scoopInstalledMullion() (string, bool) {
	if runtime.GOOS != "windows" {
		return "", false
	}
	for _, root := range scoopRoots() {
		exe := filepath.Join(root, "apps", "mullion", "current", "mullion.exe")
		if _, err := os.Stat(exe); err == nil {
			return exe, true
		}
	}
	return "", false
}

// Info is what the CLI and the panel show about updates.
type Info struct {
	Current      string    `json:"current"`
	Channel      Channel   `json:"channel"`
	ChannelLabel string    `json:"channelLabel"`
	Manual       string    `json:"manual"`
	Latest       string    `json:"latest,omitempty"`
	Tag          string    `json:"tag,omitempty"`
	URL          string    `json:"url,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	Published    time.Time `json:"published,omitempty"`
	Update       bool      `json:"update"`
	// Error explains why Latest is empty (offline, rate-limited).
	Error string `json:"error,omitempty"`
}

// Check reports the current version, the install channel and — when
// GitHub answers within ctx — the latest release. force skips the cache.
func Check(ctx context.Context, force bool) Info {
	ch := DetectChannel()
	info := Info{Current: version.Number, Channel: ch, ChannelLabel: ch.Label(), Manual: ch.ManualCommand()}
	get := Latest
	if force {
		get = LatestFresh
	}
	rel, err := get(ctx)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Latest, info.Tag, info.URL, info.Published = rel.Version(), rel.Tag, rel.URL, rel.Published
	info.Notes = excerpt(rel.Body, 600)
	info.Update = Newer(rel.Version(), version.Number)
	return info
}

// excerpt shortens release notes for display, cutting at a line break.
func excerpt(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, '\n'); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "\n…"
}
