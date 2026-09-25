package mongodb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
)

// The MongoDB Database Tools (mongodump, mongorestore, and friends) ship
// separately from the server. They're published in their own release
// index, same shape idea as the server's current.json/full.json but
// smaller (~1 MB covers every release ever, so it's fetched whole —
// no need for full.json's one-object-at-a-time streaming).
const toolsIndexURL = "https://downloads.mongodb.org/tools/db/full.json"

// toolsPinnedVersion/toolsPinnedSHA256 back a last-resort install when
// the index can't be reached: a known-good release with its sha256 for
// every platform Mullion supports, checked against the full download.
const toolsPinnedVersion = "100.19.0"

var toolsPinnedSHA256 = map[string]string{
	"macos/arm64":    "8a87276ecba707bf3c4a6f73146223ee0797c478c0ee15e7b851c63bf4299810",
	"macos/x86_64":   "131ff4ffa3b1213590e2e226bba232b6ea2c245fdf1a695a4d83f114f707fc5b",
	"windows/x86_64": "e3b9f3077374fc2bc4318f86ebf56268bac0f97f52383dc622b229bad5ba084a",
}

// toolsEntries are the only files taken from the tools archive — the
// package also ships mongoexport, mongoimport, mongostat, mongotop and
// bsondump, which Mullion has no use for.
var toolsEntries = []string{pmdir.ExeName("mongodump"), pmdir.ExeName("mongorestore")}

func toolsBaseName(version string) string { return "tools-" + version }

// toolsFallbackURL mirrors the archive naming fastdl.mongodb.org uses
// for every platform: mongodb-database-tools-<target>-<arch>-<version>.zip.
func toolsFallbackURL(version string) string {
	return fmt.Sprintf("https://fastdl.mongodb.org/tools/db/mongodb-database-tools-%s-%s-%s.zip", indexTarget, indexArch, version)
}

// toolsRelease is one downloadable Database Tools release for this
// platform.
type toolsRelease struct {
	Version string
	URL     string
	SHA256  string
}

type toolsIndexDoc struct {
	Versions []struct {
		Version   string `json:"version"`
		Downloads []struct {
			Name    string `json:"name"`
			Arch    string `json:"arch"`
			Archive struct {
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
			} `json:"archive"`
		} `json:"downloads"`
	} `json:"versions"`
}

var (
	toolsIndexMu     sync.Mutex
	toolsIndexCache  []toolsRelease
	toolsIndexLoaded bool
)

// loadToolsIndex fetches and parses full.json, keeping only the
// releases with an archive for this platform's target/arch. Cached for
// the life of the process, same as the server's release index.
func loadToolsIndex(ctx context.Context) ([]toolsRelease, error) {
	if err := platformSupported(); err != nil {
		return nil, err
	}
	toolsIndexMu.Lock()
	defer toolsIndexMu.Unlock()
	if toolsIndexLoaded {
		return toolsIndexCache, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, toolsIndexURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the MongoDB Database Tools index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %s", toolsIndexURL, resp.Status)
	}
	var doc toolsIndexDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing the MongoDB Database Tools index: %w", err)
	}
	rels := parseToolsIndex(doc, indexTarget, indexArch)
	toolsIndexCache, toolsIndexLoaded = rels, true
	return rels, nil
}

// parseToolsIndex picks, for every stable release in the index, the
// archive matching target/arch (a plain function of the decoded
// document so it's testable without a network round trip).
func parseToolsIndex(doc toolsIndexDoc, target, arch string) []toolsRelease {
	var out []toolsRelease
	for _, v := range doc.Versions {
		if !fullVersionRe.MatchString(v.Version) {
			continue
		}
		for _, d := range v.Downloads {
			if d.Name == target && d.Arch == arch && d.Archive.URL != "" {
				out = append(out, toolsRelease{Version: v.Version, URL: d.Archive.URL, SHA256: d.Archive.SHA256})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return compare(out[i].Version, out[j].Version) > 0 })
	return out
}

// latestToolsRelease resolves the release to install: the newest one
// the index offers for this platform, falling back to a pinned version
// (with its known sha256) when the index can't be reached or has
// nothing for this platform.
func latestToolsRelease(ctx context.Context) toolsRelease {
	if rels, err := loadToolsIndex(ctx); err == nil && len(rels) > 0 {
		return rels[0]
	}
	key := indexTarget + "/" + indexArch
	return toolsRelease{Version: toolsPinnedVersion, URL: toolsFallbackURL(toolsPinnedVersion), SHA256: toolsPinnedSHA256[key]}
}

// ToolsBinDir returns the bin/ directory of the newest installed
// Database Tools, or "" when none is installed.
func ToolsBinDir(paths pmdir.Paths) string {
	entries, err := os.ReadDir(baseDir(paths))
	if err != nil {
		return ""
	}
	best, bestPath := "", ""
	for _, e := range entries {
		v, ok := strings.CutPrefix(e.Name(), "tools-")
		if !e.IsDir() || !ok || !fullVersionRe.MatchString(v) {
			continue
		}
		bin := filepath.Join(baseDir(paths), e.Name(), "bin")
		if _, err := os.Stat(filepath.Join(bin, pmdir.ExeName("mongodump"))); err != nil {
			continue
		}
		if best == "" || compare(v, best) > 0 {
			best, bestPath = v, bin
		}
	}
	return bestPath
}

// ToolsInstalled reports whether mongodump/mongorestore are available.
func ToolsInstalled(paths pmdir.Paths) bool { return ToolsBinDir(paths) != "" }

// InstallTools downloads mongodump and mongorestore (the MongoDB
// Database Tools package minus everything Mullion doesn't use) into
// paths.Home/mongodb/tools-<version>/. No-op once installed. The
// archive is fetched with ranged requests (internal/mongodb/remotezip.go)
// so only the two wanted executables — a few MB — cross the network,
// the same trick the Windows server build uses for its ~0.8 GB zip.
func InstallTools(ctx context.Context, paths pmdir.Paths) error {
	if err := platformSupported(); err != nil {
		return err
	}
	if ToolsInstalled(paths) {
		return nil
	}
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return err
	}

	rel := latestToolsRelease(ctx)
	if rel.URL == "" {
		return fmt.Errorf("no MongoDB Database Tools build for this platform")
	}

	staging := filepath.Join(paths.TmpDir(), "mongodb-tools-extract")
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)

	fmt.Printf("Downloading MongoDB Database Tools %s...\n", rel.Version)
	if err := extractRemoteZip(ctx, rel.URL, staging, func(name string) bool {
		dir, base := filepath.Split(filepath.FromSlash(name))
		return filepath.Base(filepath.Clean(dir)) == "bin" && contains(toolsEntries, base)
	}); err != nil {
		return fmt.Errorf("downloading MongoDB Database Tools: %w", err)
	}

	inner, err := findDirWith(staging, "mongodump")
	if err != nil {
		return err
	}
	return moveIntoPlace(inner, filepath.Join(baseDir(paths), toolsBaseName(rel.Version)), "mongodump")
}

// BackupTo dumps every user database on the running server into dir:
// one <name>.archive.gz per database (mongodump's own archive format,
// gzip-compressed, importable with mongorestore --archive --gzip).
// Returns the files written.
func BackupTo(paths pmdir.Paths, dir string) ([]string, error) {
	bin := ToolsBinDir(paths)
	if bin == "" {
		return nil, fmt.Errorf("the MongoDB Database Tools are not installed")
	}
	dbs, err := UserDatabases(paths)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var files []string
	for i, db := range dbs {
		fmt.Printf("database %d of %d: %s\n", i+1, len(dbs), db)
		out := filepath.Join(dir, db+".archive.gz")
		cmd := proc.Quiet(filepath.Join(bin, pmdir.ExeName("mongodump")),
			"--uri", ConnectionURI(), "--db", db, "--archive="+out, "--gzip")
		if outBytes, err := cmd.CombinedOutput(); err != nil {
			os.Remove(out)
			return files, fmt.Errorf("mongodump %s: %v: %s", db, err, strings.TrimSpace(string(outBytes)))
		}
		files = append(files, out)
	}
	return files, nil
}

// RestoreFile replays a mongodump archive (as BackupTo writes) into the
// running server. --drop removes each collection the archive holds
// before recreating it, so restoring twice is safe.
func RestoreFile(paths pmdir.Paths, file string) error {
	bin := ToolsBinDir(paths)
	if bin == "" {
		return fmt.Errorf("the MongoDB Database Tools are not installed")
	}
	cmd := proc.Quiet(filepath.Join(bin, pmdir.ExeName("mongorestore")),
		"--uri", ConnectionURI(), "--archive="+file, "--gzip", "--drop")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mongorestore: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
