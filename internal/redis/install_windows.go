//go:build windows

package redis

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/archive"
	"pm/internal/pmdir"
)

func latestForPlatform(ctx context.Context) (string, error) {
	rel, err := LatestWindows(ctx)
	return rel.Version(), err
}

// Preflight has nothing to check on Windows: the build is downloaded.
func Preflight() error { return nil }

// Install downloads the newest portable Redis for Windows into
// <base>/<version>/bin and returns that version.
func Install(ctx context.Context, paths pmdir.Paths, progress Progress) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	progress("Checking for the latest Redis for Windows…")
	rel, err := LatestWindows(ctx)
	if err != nil {
		return "", err
	}
	version := rel.Version()
	if _, err := os.Stat(ServerExe(paths, version)); err == nil {
		return version, nil
	}
	asset, err := PickWindowsAsset(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(BaseDir(paths), 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(BaseDir(paths), ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)

	progress(fmt.Sprintf("Downloading Redis %s for Windows (%.0f MB)…", version, float64(asset.Size)/1e6))
	zip := filepath.Join(stage, asset.Name)
	if err := fetch(ctx, asset.URL, zip, strings.TrimPrefix(asset.Digest, "sha256:")); err != nil {
		return "", err
	}
	out := filepath.Join(stage, "out")
	if err := archive.ExtractZip(zip, out); err != nil {
		return "", fmt.Errorf("unpacking %s: %w", asset.Name, err)
	}
	// The zip holds one top folder (Redis-<v>-Windows-x64-msys2/).
	root, err := findServerDir(out)
	if err != nil {
		return "", fmt.Errorf("%s: %w", asset.Name, err)
	}
	bin := filepath.Join(stage, "ver", "bin")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(root, bin); err != nil {
		return "", err
	}
	dest := versionDir(paths, version)
	os.RemoveAll(dest)
	if err := os.Rename(filepath.Join(stage, "ver"), dest); err != nil {
		return "", err
	}
	return version, nil
}

// findServerDir finds the folder holding redis-server.exe (the root or
// one level down).
func findServerDir(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "redis-server.exe")); err == nil {
		return dir, nil
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "redis-server.exe")); e.IsDir() && err == nil {
			return sub, nil
		}
	}
	return "", fmt.Errorf("no redis-server.exe inside")
}
