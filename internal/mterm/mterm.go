// Package mterm finds, installs and drives Mullion Terminal — the
// standalone terminal app (github.com/anabiiil/mullion-terminal) the
// panel can hand "open a terminal here" off to instead of its built-in
// xterm.js terminal.
//
// Mullion Terminal is single-instance: launching it again with a folder
// opens a new tab at that folder in the existing window, so Open never
// has to talk to a running copy — it just launches the app with the
// folder as an argument.
package mterm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Overridable for tests: where the GitHub API lives and which repo
// publishes the releases.
var (
	APIBase = "https://api.github.com"
	Repo    = "anabiiil/mullion-terminal"
)

// AppName is the product name: the macOS bundle is "<AppName>.app", the
// Windows executable "<AppName>.exe".
const AppName = "Mullion Terminal"

// BundleID is the macOS bundle identifier, used to find the app with
// Spotlight wherever the user put it.
const BundleID = "dev.mullion.terminal"

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// Asset is one downloadable file of a GitHub release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is the subset of GitHub's release JSON Mullion needs.
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// Version is the release tag without its leading "v".
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// Progress reports install progress: stage is "download" (done/total
// are bytes; total is 0 when unknown) or "install" (done/total unused).
type Progress func(stage string, done, total int64)

// assetSuffix is the end of the release asset name built for goos/goarch
// (electron-builder names them Mullion-Terminal-<ver>-<os>-<arch>.<ext>),
// or "" when no build exists for that platform — notably Intel Macs.
func assetSuffix(goos, goarch string) string {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "-mac-arm64.zip"
	case goos == "windows" && goarch == "amd64":
		return "-win-x64.exe"
	}
	return ""
}

// pickAsset chooses the release asset for goos/goarch.
func pickAsset(assets []Asset, goos, goarch string) (Asset, bool) {
	suffix := assetSuffix(goos, goarch)
	if suffix == "" {
		return Asset{}, false
	}
	for _, a := range assets {
		if strings.HasPrefix(a.Name, "Mullion-Terminal-") && strings.HasSuffix(a.Name, suffix) {
			return a, true
		}
	}
	return Asset{}, false
}

// Available reports whether a Mullion Terminal build is published for
// this OS/architecture (Apple-silicon macOS and 64-bit Windows).
func Available() bool { return assetSuffix(runtime.GOOS, runtime.GOARCH) != "" }

// UnavailableReason explains, for the UI, why Available is false.
func UnavailableReason() string {
	if Available() {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return "Mullion Terminal is built for Apple-silicon Macs only."
	}
	return "Mullion Terminal isn't published for this platform."
}

// Latest fetches the latest release's metadata from GitHub.
func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("checking for Mullion Terminal releases: HTTP %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("reading the Mullion Terminal release: %w", err)
	}
	if rel.Tag == "" {
		return Release{}, errors.New("the latest Mullion Terminal release has no version tag")
	}
	return rel, nil
}

// latestCache keeps the last successful LatestVersion lookup for a
// while: the panel asks for status every time Settings opens, and the
// unauthenticated GitHub API allows only 60 requests an hour.
var latestCache struct {
	sync.Mutex
	version string
	at      time.Time
}

// LatestVersion is a cached, best-effort Latest().Version().
func LatestVersion(ctx context.Context) (string, error) {
	latestCache.Lock()
	if latestCache.version != "" && time.Since(latestCache.at) < 30*time.Minute {
		v := latestCache.version
		latestCache.Unlock()
		return v, nil
	}
	latestCache.Unlock()
	rel, err := Latest(ctx)
	if err != nil {
		return "", err
	}
	latestCache.Lock()
	latestCache.version, latestCache.at = rel.Version(), time.Now()
	latestCache.Unlock()
	return rel.Version(), nil
}

// Status is what the panel shows about Mullion Terminal.
type Status struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	// Latest is the newest published version ("" when GitHub couldn't
	// be reached within the timeout); Update is true when it's newer
	// than the installed one.
	Latest string `json:"latest,omitempty"`
	Update bool   `json:"update"`
}

// GetStatus reports availability and the installed copy; with
// checkLatest it also asks GitHub (bounded by ctx) for the newest
// version.
func GetStatus(ctx context.Context, checkLatest bool) Status {
	st := Status{Available: Available(), Reason: UnavailableReason()}
	if p, ok := Find(); ok {
		st.Installed, st.Path, st.Version = true, p, InstalledVersion(p)
	}
	if checkLatest && st.Available {
		if v, err := LatestVersion(ctx); err == nil {
			st.Latest = v
			st.Update = st.Installed && st.Version != "" && newer(v, st.Version)
		}
	}
	return st
}

// Install downloads the latest release for this platform and installs
// it (replacing an existing copy), returning the installed version.
func Install(ctx context.Context, progress Progress) (string, error) {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	if !Available() {
		return "", errors.New(UnavailableReason())
	}
	rel, err := Latest(ctx)
	if err != nil {
		return "", err
	}
	asset, ok := pickAsset(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("Mullion Terminal %s has no download for %s/%s", rel.Version(), runtime.GOOS, runtime.GOARCH)
	}
	tmp, err := os.MkdirTemp("", "mullion-terminal-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	file := filepath.Join(tmp, asset.Name)
	if err := fetch(ctx, asset.URL, file, func(done, total int64) { progress("download", done, total) }); err != nil {
		return "", err
	}
	progress("install", 0, 0)
	if err := installFile(ctx, file, tmp); err != nil {
		return "", err
	}
	p, ok := Find()
	if !ok {
		return "", errors.New("the installer finished but Mullion Terminal wasn't found afterwards")
	}
	latestCache.Lock()
	latestCache.version, latestCache.at = rel.Version(), time.Now()
	latestCache.Unlock()
	if v := InstalledVersion(p); v != "" {
		return v, nil
	}
	return rel.Version(), nil
}

// Open opens dir as a new tab in Mullion Terminal, launching it when
// it isn't running. It returns as soon as the launch is handed off.
func Open(dir string) error {
	p, ok := Find()
	if !ok {
		return errors.New("Mullion Terminal is not installed — install it from Settings → Terminal, or run: mullion terminal install")
	}
	if dir != "" {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", dir)
		}
	}
	return launch(p, dir)
}

// fetch downloads url into dest, reporting progress about every 150ms.
func fetch(ctx context.Context, url, dest string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading Mullion Terminal: HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	pw := &progressWriter{total: resp.ContentLength, report: progress}
	_, err = io.Copy(io.MultiWriter(f, pw), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading Mullion Terminal: %w", err)
	}
	progress(pw.done, pw.total)
	return nil
}

type progressWriter struct {
	done, total int64
	last        time.Time
	report      func(done, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if time.Since(w.last) > 150*time.Millisecond {
		w.last = time.Now()
		w.report(w.done, w.total)
	}
	return len(p), nil
}

// newer reports whether version a is newer than b, comparing dotted
// numeric parts ("0.1.10" > "0.1.9"); a pre-release suffix is ignored.
func newer(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}
