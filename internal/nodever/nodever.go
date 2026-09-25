// Package nodever installs and resolves Node.js versions from the
// official nodejs.org builds — same pattern as the PHP versions: one
// directory per version, a `current` junction for the global one.
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
	"time"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
)

const distBase = "https://nodejs.org/dist/"

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// Release is one downloadable Node build for this platform.
type Release struct {
	Version string // "22.12.0" (no leading v)
	LTS     string // codename ("Jod") or "" for current releases
	URL     string
}

// platform returns nodejs.org's file suffix and archive extension.
func platform() (suffix, ext string, err error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		return "", "", fmt.Errorf("unsupported CPU architecture %s", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "windows":
		return "win-" + arch, ".zip", nil
	case "darwin":
		return "darwin-" + arch, ".tar.gz", nil
	case "linux":
		return "linux-" + arch, ".tar.gz", nil
	}
	return "", "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

// FetchAll lists every downloadable version for this platform, newest first.
func FetchAll(ctx context.Context) ([]Release, error) {
	suffix, ext, err := platform()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, distBase+"index.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodejs.org index: HTTP %s", resp.Status)
	}
	var raw []struct {
		Version string   `json:"version"`
		LTS     any      `json:"lts"` // false or "Jod"
		Files   []string `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing the nodejs.org index: %w", err)
	}
	// index.json's files entries name the platform without extension for
	// tarballs ("osx-arm64-tar" historically, "linux-x64", "win-x64-zip").
	fileKeys := map[string]bool{}
	switch {
	case strings.HasPrefix(suffix, "win-"):
		fileKeys[suffix+"-zip"] = true
	case strings.HasPrefix(suffix, "darwin-"):
		fileKeys["osx-"+strings.TrimPrefix(suffix, "darwin-")+"-tar"] = true
	default:
		fileKeys[suffix] = true
	}
	var out []Release
	for _, r := range raw {
		ok := false
		for _, f := range r.Files {
			if fileKeys[f] {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		v := strings.TrimPrefix(r.Version, "v")
		lts := ""
		if name, isStr := r.LTS.(string); isStr {
			lts = name
		}
		out = append(out, Release{
			Version: v,
			LTS:     lts,
			URL:     fmt.Sprintf("%sv%s/node-v%s-%s%s", distBase, v, v, suffix, ext),
		})
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i].Version, out[j].Version) > 0 })
	return out, nil
}

// Resolve turns a user argument into a downloadable release:
// "lts" (newest LTS), "latest", "22", "22.12", or "22.12.0".
func Resolve(ctx context.Context, arg string) (Release, error) {
	all, err := FetchAll(ctx)
	if err != nil {
		return Release{}, err
	}
	if len(all) == 0 {
		return Release{}, fmt.Errorf("no Node builds found for this platform")
	}
	arg = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "v")))
	switch arg {
	case "", "lts":
		for _, r := range all {
			if r.LTS != "" {
				return r, nil
			}
		}
		return Release{}, fmt.Errorf("no LTS release found")
	case "latest", "current":
		return all[0], nil
	}
	sel, err := ParseSelector(arg)
	if err != nil {
		return Release{}, fmt.Errorf("invalid version %q — use e.g. 22, 22.12.0, lts, or latest", arg)
	}
	for _, r := range all { // newest first
		if sel.Matches(r.Version) {
			return r, nil
		}
	}
	return Release{}, fmt.Errorf("no Node %s build found for this platform", arg)
}

// Install downloads and unpacks a release; no-op when already installed.
func Install(ctx context.Context, paths pmdir.Paths, rel Release) (string, error) {
	destDir := paths.NodeVersionDir(rel.Version)
	if _, err := os.Stat(NodeBin(destDir)); err == nil {
		return destDir, nil
	}
	archivePath := filepath.Join(paths.TmpDir(), filepath.Base(rel.URL))
	fmt.Printf("Downloading Node %s...\n", rel.Version)
	if err := download.ToFile(ctx, rel.URL, archivePath); err != nil {
		return "", err
	}
	defer os.Remove(archivePath)

	staging := filepath.Join(paths.TmpDir(), "node-extract")
	os.RemoveAll(staging)
	var err error
	if strings.HasSuffix(rel.URL, ".zip") {
		err = archive.ExtractZip(archivePath, staging)
	} else {
		err = archive.ExtractTarGz(archivePath, staging)
	}
	if err != nil {
		return "", fmt.Errorf("extracting %s: %w", archivePath, err)
	}
	inner, err := findInner(staging)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.NodeDir(), 0o755); err != nil {
		return "", err
	}
	os.RemoveAll(destDir)
	if err := os.Rename(inner, destDir); err != nil {
		return "", err
	}
	os.RemoveAll(staging)
	return destDir, nil
}

// findInner locates the extracted directory holding the node binary.
func findInner(staging string) (string, error) {
	candidates := []string{staging}
	if entries, err := os.ReadDir(staging); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(staging, e.Name()))
			}
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(NodeBin(dir)); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("unexpected archive layout: no node binary under %s", staging)
}

// Installed lists the versions present under ~/.mullion/node, newest first.
func Installed(paths pmdir.Paths) ([]string, error) {
	entries, err := os.ReadDir(paths.NodeDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "current" {
			continue
		}
		if _, err := ParseSelector(e.Name()); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i], out[j]) > 0 })
	return out, nil
}

// FindInstalled returns the newest installed version matching the selector.
func FindInstalled(paths pmdir.Paths, arg string) (string, error) {
	versions, err := Installed(paths)
	if err != nil {
		return "", err
	}
	arg = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "v")))
	if arg == "" || arg == "lts" || arg == "latest" || arg == "current" {
		if len(versions) == 0 {
			return "", fmt.Errorf("no Node versions installed (run: mullion node install lts)")
		}
		return versions[0], nil
	}
	sel, err := ParseSelector(arg)
	if err != nil {
		return "", fmt.Errorf("invalid version %q", arg)
	}
	for _, v := range versions {
		if sel.Matches(v) {
			return v, nil
		}
	}
	return "", fmt.Errorf("Node %s is not installed (run: mullion node install %s)", arg, arg)
}

// Selector is a possibly-partial version: "22", "22.12", "22.12.0".
type Selector struct{ Major, Minor, Patch int }

func ParseSelector(s string) (Selector, error) {
	sel := Selector{Major: -1, Minor: -1, Patch: -1}
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(s, "v")), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return sel, fmt.Errorf("invalid version %q", s)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return sel, fmt.Errorf("invalid version %q", s)
		}
		nums[i] = n
	}
	sel.Major = nums[0]
	if len(nums) > 1 {
		sel.Minor = nums[1]
	}
	if len(nums) > 2 {
		sel.Patch = nums[2]
	}
	return sel, nil
}

func (sel Selector) Matches(full string) bool {
	v, err := ParseSelector(full)
	if err != nil || v.Patch == -1 {
		return false
	}
	if sel.Major != v.Major {
		return false
	}
	if sel.Minor != -1 && sel.Minor != v.Minor {
		return false
	}
	if sel.Patch != -1 && sel.Patch != v.Patch {
		return false
	}
	return true
}

// Compare orders two full versions ("22.9.0" < "22.12.0").
func Compare(a, b string) int {
	av, _ := ParseSelector(a)
	bv, _ := ParseSelector(b)
	for _, pair := range [][2]int{{av.Major, bv.Major}, {av.Minor, bv.Minor}, {av.Patch, bv.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}
