package mongodb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

// Install downloads mongod for the version (and mongosh, if no shell is
// installed yet). It is a no-op when both are present.
func Install(ctx context.Context, paths pmdir.Paths, version string) error {
	if err := platformSupported(); err != nil {
		return err
	}
	if !fullVersionRe.MatchString(version) {
		return fmt.Errorf("invalid MongoDB version %q (resolve it first)", version)
	}
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(serverExe(paths, version)); err != nil {
		if err := installServer(ctx, paths, version); err != nil {
			return err
		}
	}
	if MongoshPath(paths) == "" {
		if err := installMongosh(ctx, paths); err != nil {
			return err
		}
	}
	return nil
}

func installServer(ctx context.Context, paths pmdir.Paths, version string) error {
	url, sum := serverFallbackURL(version), ""
	if rel, ok := lookupRelease(ctx, version); ok {
		url, sum = rel.URL, rel.SHA256
	}
	staging := filepath.Join(paths.TmpDir(), "mongodb-extract")
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)

	fmt.Printf("Downloading %s...\n", Label(version))
	if len(serverEntries) > 0 {
		// The Windows zip is ~0.8 GB, almost all of it debug symbols:
		// fetch just the executables out of it with range requests.
		// Each entry is CRC-checked as it's inflated.
		if err := extractRemoteZip(ctx, url, staging, func(name string) bool {
			dir, base := filepath.Split(filepath.FromSlash(name))
			return filepath.Base(filepath.Clean(dir)) == "bin" && contains(serverEntries, base)
		}); err != nil {
			return fmt.Errorf("downloading %s: %w", Label(version), err)
		}
	} else if err := fetchAndExtract(ctx, paths, url, sum, staging); err != nil {
		return fmt.Errorf("downloading %s: %w", Label(version), err)
	}

	inner, err := findDirWith(staging, "mongod")
	if err != nil {
		return err
	}
	return moveIntoPlace(inner, versionDir(paths, version), "mongod")
}

func installMongosh(ctx context.Context, paths pmdir.Paths) error {
	rel := latestMongosh(ctx)
	staging := filepath.Join(paths.TmpDir(), "mongosh-extract")
	os.RemoveAll(staging)
	defer os.RemoveAll(staging)

	fmt.Printf("Downloading mongosh %s...\n", rel.Version)
	if err := fetchAndExtract(ctx, paths, rel.URL, rel.SHA256, staging); err != nil {
		return fmt.Errorf("downloading mongosh: %w", err)
	}
	inner, err := findDirWith(staging, "mongosh")
	if err != nil {
		return err
	}
	return moveIntoPlace(inner, filepath.Join(baseDir(paths), "mongosh-"+rel.Version), "mongosh")
}

// fetchAndExtract downloads an archive into the tmp dir, checks its
// sha256 when the index published one, and unpacks it into staging.
func fetchAndExtract(ctx context.Context, paths pmdir.Paths, url, sum, staging string) error {
	archivePath := filepath.Join(paths.TmpDir(), filepath.Base(url))
	if err := download.ToFile(ctx, url, archivePath); err != nil {
		return err
	}
	defer os.Remove(archivePath)
	if sum != "" {
		if err := checkSHA256(archivePath, sum); err != nil {
			return err
		}
	}
	var err error
	switch {
	case strings.HasSuffix(url, ".zip"):
		err = archive.ExtractZip(archivePath, staging)
	case strings.HasSuffix(url, ".tgz"), strings.HasSuffix(url, ".tar.gz"):
		err = archive.ExtractTarGz(archivePath, staging)
	default:
		err = fmt.Errorf("unsupported archive type: %s", filepath.Base(url))
	}
	if err != nil {
		return fmt.Errorf("extracting %s: %w", filepath.Base(archivePath), err)
	}
	return nil
}

func checkSHA256(file, want string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s)", filepath.Base(file), got, want)
	}
	return nil
}

// findDirWith locates the extracted directory holding bin/<tool> — more
// robust than predicting the archive's top-level directory name.
func findDirWith(staging, tool string) (string, error) {
	candidates := []string{staging}
	if entries, err := os.ReadDir(staging); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(staging, e.Name()))
			}
		}
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "bin", pmdir.ExeName(tool))); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("unexpected archive layout: no bin/%s under %s", pmdir.ExeName(tool), staging)
}

// moveIntoPlace makes the extracted binaries runnable, checks that the
// tool actually starts, and moves the directory to its final path.
func moveIntoPlace(src, dest, tool string) error {
	prepareBinaries(src)
	if err := checkRuns(filepath.Join(src, "bin", pmdir.ExeName(tool))); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	os.RemoveAll(dest) // a half-finished earlier attempt
	return os.Rename(src, dest)
}

// checkRuns executes `<tool> --version` so a broken download (or, on
// Windows, a missing Visual C++ runtime) fails the install instead of
// the first start.
func checkRuns(exe string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := withContext(ctx, proc.Quiet(exe, "--version"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s does not run: %v: %s%s", filepath.Base(exe), err,
			strings.TrimSpace(string(out)), runHint)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
