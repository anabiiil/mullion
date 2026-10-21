//go:build !windows

package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"pm/internal/archive"
	"pm/internal/pmdir"
)

// ErrToolsMissing is returned when there is no compiler to build Redis.
var ErrToolsMissing = errors.New("building Redis needs a C compiler and make — on macOS install the Xcode Command Line Tools (run: xcode-select --install), then try again")

func latestForPlatform(ctx context.Context) (string, error) {
	rel, err := LatestSource(ctx)
	return rel.Version, err
}

// Preflight checks the build tools are there without installing (or
// prompting to install) anything: on macOS /usr/bin/cc and make are
// stubs that open the Command Line Tools installer when the tools are
// missing, so xcode-select is asked first.
func Preflight() error {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("xcode-select", "-p").Output()
		dir := strings.TrimSpace(string(out))
		if err != nil || dir == "" {
			return ErrToolsMissing
		}
		if _, err := os.Stat(dir); err != nil {
			return ErrToolsMissing
		}
		if err := exec.Command("xcrun", "--find", "cc").Run(); err != nil {
			return ErrToolsMissing
		}
	}
	for _, tool := range []string{"cc", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%w (%s not found)", ErrToolsMissing, tool)
		}
	}
	return nil
}

// Install builds the newest stable Redis from source into
// <base>/<version>/bin and returns that version. An already-built
// version is not built again.
func Install(ctx context.Context, paths pmdir.Paths, progress Progress) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	progress("Checking for the latest Redis…")
	rel, err := LatestSource(ctx)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(ServerExe(paths, rel.Version)); err == nil {
		return rel.Version, nil
	}
	if err := Preflight(); err != nil {
		return "", err
	}
	return buildRelease(ctx, paths, rel, progress)
}

// buildRelease downloads, verifies and builds one source release.
func buildRelease(ctx context.Context, paths pmdir.Paths, rel SourceRelease, progress Progress) (string, error) {
	for _, d := range []string{BaseDir(paths), paths.LogsDir(), paths.TmpDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	work, err := os.MkdirTemp(paths.TmpDir(), "redis-build-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)

	progress("Downloading the Redis " + rel.Version + " source…")
	tarball := filepath.Join(work, "redis-"+rel.Version+".tar.gz")
	if err := fetch(ctx, rel.URL, tarball, rel.SHA256); err != nil {
		return "", err
	}
	src := filepath.Join(work, "src")
	if err := archive.ExtractTarGz(tarball, src); err != nil {
		return "", fmt.Errorf("unpacking the Redis source: %w", err)
	}
	tree := filepath.Join(src, "redis-"+rel.Version)
	if _, err := os.Stat(filepath.Join(tree, "src", "Makefile")); err != nil {
		return "", fmt.Errorf("the Redis %s source has no src/Makefile", rel.Version)
	}

	progress("Building Redis " + rel.Version + " (about a minute)…")
	logf, err := os.Create(BuildLog(paths))
	if err != nil {
		return "", err
	}
	defer logf.Close()
	// Only the core server and CLI: the top-level Makefile of Redis 8
	// also builds the bundled modules (search, JSON, …), which need
	// Rust/CMake toolchains — `make -C src` skips them. No TLS (that
	// would need OpenSSL headers); the system allocator on macOS.
	jobs := runtime.NumCPU()
	if jobs > 8 {
		jobs = 8
	}
	cmd := exec.CommandContext(ctx, "make", "-C", "src", "-j"+strconv.Itoa(jobs), "BUILD_TLS=no", "redis-server", "redis-cli")
	cmd.Dir = tree
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Env = append(os.Environ(), "MAKEFLAGS=")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building Redis failed (%v)%s — the full log is %s", err, tailOf(BuildLog(paths), 6), BuildLog(paths))
	}

	stage, err := os.MkdirTemp(BaseDir(paths), ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	bin := filepath.Join(stage, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"redis-server", "redis-cli"} {
		if err := copyFile(filepath.Join(tree, "src", name), filepath.Join(bin, name), 0o755); err != nil {
			return "", fmt.Errorf("installing %s: %w", name, err)
		}
	}
	// The license travels with the binaries.
	for _, name := range []string{"LICENSE.txt", "COPYING"} {
		if _, err := os.Stat(filepath.Join(tree, name)); err == nil {
			_ = copyFile(filepath.Join(tree, name), filepath.Join(stage, name), 0o644)
		}
	}
	if out, err := exec.Command(filepath.Join(bin, "redis-server"), "--version").Output(); err != nil || !strings.Contains(string(out), "v="+rel.Version) {
		return "", fmt.Errorf("the built redis-server does not run (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	dest := versionDir(paths, rel.Version)
	os.RemoveAll(dest)
	if err := os.Rename(stage, dest); err != nil {
		return "", err
	}
	return rel.Version, nil
}

// tailOf is the last n lines of a file, indented, for error messages.
func tailOf(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return ":\n  " + strings.Join(lines, "\n  ")
}
