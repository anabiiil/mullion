//go:build darwin

package postgres

import (
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// edbPlatform is the platform part of EDB's binary zip names. The macOS
// zips are universal (arm64 + x86_64), so one file serves both CPUs.
const edbPlatform = "osx"

func platformSupported() error { return nil }

// prepareBinaries makes a freshly extracted tree runnable: Go's HTTP
// client never sets the quarantine attribute, but clear it anyway in
// case the archive was fetched some other way (Gatekeeper would
// otherwise block every binary), and make sure bin/ is executable.
func prepareBinaries(dir string) {
	_ = exec.Command("xattr", "-dr", "com.apple.quarantine", dir).Run()
	chmodBin(dir)
	dedupeDylibs(filepath.Join(dir, "lib"))
}

// dedupeDylibs turns the zip's duplicate library copies back into the
// symlinks they were on EDB's build machine (libicudata.dylib,
// libicudata.68.dylib and libicudata.68.2.dylib are three identical
// 57 MB files in the zip): identical content only, the most specific
// name stays a real file. Saves ~180 MB per version.
func dedupeDylibs(lib string) {
	entries, err := os.ReadDir(lib)
	if err != nil {
		return
	}
	groups := map[[32]byte][]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".dylib") {
			continue
		}
		f, err := os.Open(filepath.Join(lib, e.Name()))
		if err != nil {
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			continue
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		groups[sum] = append(groups[sum], e.Name())
	}
	for _, names := range groups {
		if len(names) < 2 {
			continue
		}
		keep := names[0]
		for _, n := range names[1:] {
			if len(n) > len(keep) {
				keep = n
			}
		}
		for _, n := range names {
			if n == keep {
				continue
			}
			p := filepath.Join(lib, n)
			tmp := p + ".mullion-link"
			if os.Symlink(keep, tmp) == nil && os.Rename(tmp, p) != nil {
				os.Remove(tmp)
			}
		}
	}
}

// chmodBin adds the execute bits to everything in bin/ — archive
// extraction preserves the zip's modes, but be defensive.
func chmodBin(dir string) {
	entries, err := os.ReadDir(filepath.Join(dir, "bin"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			p := filepath.Join(dir, "bin", e.Name())
			if info, err := os.Stat(p); err == nil {
				_ = os.Chmod(p, info.Mode().Perm()|0o755)
			}
		}
	}
}

// runHint is appended to "does not run" install errors.
const runHint = ""
