//go:build darwin

package mongodb

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// indexTarget/indexArch select this platform's builds in MongoDB's
// release index (downloads.mongodb.org/current.json).
const indexTarget = "macos"

var indexArch = map[string]string{"arm64": "arm64", "amd64": "x86_64"}[runtime.GOARCH]

// mongoshDistro is this platform's "distro" in mongosh's release index.
const mongoshDistro = "darwin"

// serverEntries is empty: the macOS tarball (~80 MB) is downloaded whole
// and its sha256 checked against the index.
var serverEntries []string

// runHint is appended when an installed binary fails to execute.
const runHint = ""

func platformSupported() error {
	if indexArch == "" {
		return fmt.Errorf("MongoDB has no macOS builds for %s", runtime.GOARCH)
	}
	return nil
}

// serverFallbackURL is the official archive URL, used when the release
// index can't be reached.
func serverFallbackURL(version string) string {
	return fmt.Sprintf("https://fastdl.mongodb.org/osx/mongodb-macos-%s-%s.tgz", indexArch, version)
}

func mongoshURL(version string) string {
	arch := map[string]string{"arm64": "arm64", "x86_64": "x64"}[indexArch]
	return fmt.Sprintf("https://downloads.mongodb.com/compass/mongosh-%s-darwin-%s.zip", version, arch)
}

// prepareBinaries clears the download quarantine flag (Gatekeeper would
// otherwise block the unsigned-by-us binaries) and makes bin/* executable
// — the zip extractor doesn't carry file modes over from mongosh's zip.
func prepareBinaries(dir string) {
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", dir).Run()
	entries, err := os.ReadDir(filepath.Join(dir, "bin"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			_ = os.Chmod(filepath.Join(dir, "bin", e.Name()), 0o755)
		}
	}
}
