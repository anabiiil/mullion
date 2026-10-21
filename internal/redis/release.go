package redis

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Where releases are looked up; variables so tests can point them at
// an httptest server.
var (
	// HashesURL is the official list of every source release and its
	// SHA-256 (github.com/redis/redis-hashes).
	HashesURL = "https://raw.githubusercontent.com/redis/redis-hashes/master/README"
	// WindowsAPI is the GitHub releases endpoint of the portable Windows
	// builds.
	WindowsAPI = "https://api.github.com/repos/redis-windows/redis-windows/releases/latest"
)

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// SourceRelease is one official source tarball.
type SourceRelease struct {
	Version string
	SHA256  string
	URL     string
}

// hashLineRe matches a redis-hashes line:
//
//	hash redis-8.10.2.tar.gz sha256 b9ff…02b3 http://download.redis.io/releases/redis-8.10.2.tar.gz
var hashLineRe = regexp.MustCompile(`^hash\s+redis-(\d+\.\d+\.\d+)\.tar\.gz\s+sha256\s+([0-9a-f]{64})\s+(\S+)`)

// ParseHashes picks the newest stable release (release candidates and
// milestones never match the x.y.z pattern) from redis-hashes' README.
// The URL is upgraded to https.
func ParseHashes(text string) (SourceRelease, error) {
	var best SourceRelease
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		m := hashLineRe.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		if best.Version == "" || Newer(m[1], best.Version) {
			best = SourceRelease{Version: m[1], SHA256: m[2], URL: strings.Replace(m[3], "http://", "https://", 1)}
		}
	}
	if best.Version == "" {
		return best, fmt.Errorf("no Redis release found in the published hashes")
	}
	return best, nil
}

// LatestSource looks up the newest stable source release.
func LatestSource(ctx context.Context) (SourceRelease, error) {
	body, err := get(ctx, HashesURL, "")
	if err != nil {
		return SourceRelease{}, fmt.Errorf("checking for Redis releases: %w", err)
	}
	return ParseHashes(body)
}

// Asset is one downloadable file of a GitHub release.
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // "sha256:<hex>" (GitHub fills it in)
}

// WindowsRelease is the subset of GitHub's release JSON Mullion needs.
type WindowsRelease struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// Version is the release tag without a leading "v".
func (r WindowsRelease) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// PickWindowsAsset chooses the portable zip of a redis-windows release:
// the MSYS2 build without the .NET service wrapper
// ("Redis-<v>-Windows-x64-msys2.zip"), else the Cygwin one. Both are
// x64 — Windows on Arm runs them under emulation.
func PickWindowsAsset(rel WindowsRelease) (Asset, error) {
	v := rel.Version()
	for _, want := range []string{
		"Redis-" + v + "-Windows-x64-msys2.zip",
		"Redis-" + v + "-Windows-x64-cygwin.zip",
	} {
		for _, a := range rel.Assets {
			if strings.EqualFold(a.Name, want) {
				return a, nil
			}
		}
	}
	names := make([]string, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		names = append(names, a.Name)
	}
	return Asset{}, fmt.Errorf("the latest Redis for Windows release (%s) has no portable x64 zip (assets: %s)", rel.Tag, strings.Join(names, ", "))
}

// LatestWindows fetches the newest redis-windows release's metadata.
func LatestWindows(ctx context.Context) (WindowsRelease, error) {
	body, err := get(ctx, WindowsAPI, "application/vnd.github+json")
	if err != nil {
		return WindowsRelease{}, fmt.Errorf("checking for Redis for Windows releases: %w", err)
	}
	var rel WindowsRelease
	if err := json.Unmarshal([]byte(body), &rel); err != nil {
		return rel, fmt.Errorf("reading the Redis for Windows release: %w", err)
	}
	if !fullVersionRe.MatchString(rel.Version()) {
		return rel, fmt.Errorf("unexpected Redis for Windows release tag %q", rel.Tag)
	}
	return rel, nil
}

// latestCache keeps the last lookup for a while: the panel asks when the
// Redis tab opens, and GitHub's unauthenticated API allows 60 calls an
// hour.
var latestCache struct {
	sync.Mutex
	version string
	at      time.Time
}

// LatestVersion is a cached, best-effort lookup of the version Install
// would install on this platform.
func LatestVersion(ctx context.Context) (string, error) {
	latestCache.Lock()
	if latestCache.version != "" && time.Since(latestCache.at) < 30*time.Minute {
		v := latestCache.version
		latestCache.Unlock()
		return v, nil
	}
	latestCache.Unlock()
	v, err := latestForPlatform(ctx)
	if err != nil {
		return "", err
	}
	latestCache.Lock()
	latestCache.version, latestCache.at = v, time.Now()
	latestCache.Unlock()
	return v, nil
}

func get(ctx context.Context, url, accept string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

// fetch downloads url into dest and checks its SHA-256 (hex; "" = no
// check).
func fetch(ctx context.Context, url, dest, sum string) error {
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
		return fmt.Errorf("downloading %s: HTTP %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); sum != "" && !strings.EqualFold(got, sum) {
		os.Remove(dest)
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s) — the download is corrupt or was tampered with", url, got, sum)
	}
	return nil
}
