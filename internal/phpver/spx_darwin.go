//go:build darwin

package phpver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pm/internal/download"
	"pm/internal/pmdir"
)

// ErrSpxToolsMissing is returned by EnsureSpx when the Command Line
// Tools (clang, make, the macOS SDK's zlib) aren't available.
var ErrSpxToolsMissing = errors.New("the Xcode Command Line Tools are required to build the profiler (run: xcode-select --install)")

// SpxSupported reports whether EnsureSpx can build SPX here.
func SpxSupported() bool { return true }

// EnsureSpx makes the SPX profiler available to the PHP version
// installed at paths.PhpVersionDir(version): spx.so built from SPX's
// release tarball against the matching php.net sources, loaded from the
// version's php.ini with SPX's defaults (disabled — sites turn it on
// per request through Caddy, see internal/profiler), and SPX's web UI
// unpacked once into SpxAssetsDir.
//
// It is a no-op when php.ini already loads a working spx.so. Like
// EnsureLdap it installs nothing system-wide: it only runs xcrun,
// clang/make (php-src's configure), tar, and mullion's download helper.
func EnsureSpx(ctx context.Context, paths pmdir.Paths, version string) error {
	verDir := paths.PhpVersionDir(version)
	php := filepath.Join(verDir, PhpExeName)
	iniPath := filepath.Join(verDir, "php.ini")
	destSo := SpxSoPath(paths, version)

	if _, err := os.Stat(php); err != nil {
		return fmt.Errorf("PHP %s is not installed", version)
	}
	// A module built earlier (loaded, or switched off) is reused.
	if fileExists(destSo) && fileExists(filepath.Join(SpxAssetsDir(paths), "index.html")) &&
		verifySharedExt(php, destSo, "spx") == nil {
		return setSharedExtLine(iniPath, destSo, spxIniComment, true)
	}
	sdk, err := preflightSpxTools()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(paths.LogsDir(), 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(paths.LogsDir(), fmt.Sprintf("php-%s-spx-build.log", version))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("creating SPX build log %s: %w", logPath, err)
	}
	defer logFile.Close()

	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp(paths.TmpDir(), "spx-build-*")
	if err != nil {
		return fmt.Errorf("creating SPX build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	spxSrc, err := fetchSpxSource(ctx, buildDir, logFile, logPath)
	if err != nil {
		return err
	}
	if err := installSpxAssets(paths, spxSrc); err != nil {
		return err
	}
	if fileExists(destSo) && verifySharedExt(php, destSo, "spx") == nil {
		return setSharedExtLine(iniPath, destSo, spxIniComment, true)
	}

	fmt.Printf("Adding the SPX profiler to PHP %s (builds spx.so from source, ~1 min)...\n", version)
	soPath, err := buildSpx(ctx, version, sdk, spxSrc, SpxAssetsDir(paths), buildDir, logFile, logPath)
	if err != nil {
		return err
	}
	if err := copyFileMode(soPath, destSo, 0o755); err != nil {
		return fmt.Errorf("installing %s: %w", destSo, err)
	}
	if err := verifySharedExt(php, destSo, "spx"); err != nil {
		os.Remove(destSo)
		return err
	}
	if err := setSharedExtLine(iniPath, destSo, spxIniComment, true); err != nil {
		return fmt.Errorf("enabling spx.so in %s: %w", iniPath, err)
	}
	return nil
}

// DisableSpx comments out the version's spx.so line (the module stays
// on disk, so EnsureSpx can switch it back on without a rebuild).
func DisableSpx(paths pmdir.Paths, version string) error {
	return setSharedExtLine(filepath.Join(paths.PhpVersionDir(version), "php.ini"), SpxSoPath(paths, version), "", false)
}

// preflightSpxTools checks for clang, make, tar, xcrun and the SDK's
// zlib without installing anything, returning the SDK path.
func preflightSpxTools() (string, error) {
	for _, tool := range []string{"clang", "make", "tar", "xcrun"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "", fmt.Errorf("%w (%s not found)", ErrSpxToolsMissing, tool)
		}
	}
	out, err := exec.Command("xcrun", "--show-sdk-path").Output()
	if err != nil {
		return "", fmt.Errorf("%w (xcrun --show-sdk-path failed: %v)", ErrSpxToolsMissing, err)
	}
	sdk := strings.TrimSpace(string(out))
	if sdk == "" {
		return "", fmt.Errorf("%w (xcrun --show-sdk-path returned nothing)", ErrSpxToolsMissing)
	}
	for _, rel := range []string{
		filepath.Join("usr", "include", "zlib.h"),
		filepath.Join("usr", "lib", "libz.tbd"),
	} {
		if _, err := os.Stat(filepath.Join(sdk, rel)); err != nil {
			return "", fmt.Errorf("%w (missing %s)", ErrSpxToolsMissing, filepath.Join(sdk, rel))
		}
	}
	return sdk, nil
}

// fetchSpxSource downloads and checks the pinned SPX tarball into
// buildDir and returns the extracted source directory.
func fetchSpxSource(ctx context.Context, buildDir string, logFile *os.File, logPath string) (string, error) {
	tarPath := filepath.Join(buildDir, "php-spx-"+SpxVersion+".tar.gz")
	if err := download.ToFile(ctx, spxURL, tarPath); err != nil {
		return "", err
	}
	if err := verifySHA256(tarPath, spxSHA256); err != nil {
		return "", err
	}
	if err := runLogged(ctx, buildDir, logFile, "tar", "-xzf", tarPath, "-C", buildDir); err != nil {
		return "", sqlsrvBuildError("SPX", err, logPath)
	}
	os.Remove(tarPath)
	src := filepath.Join(buildDir, "php-spx-"+SpxVersion)
	if !fileExists(filepath.Join(src, "src", "php_spx.c")) {
		return "", sqlsrvBuildError("SPX", fmt.Errorf("unexpected SPX archive layout (no %s)", filepath.Join(src, "src", "php_spx.c")), logPath)
	}
	return src, nil
}

// installSpxAssets copies SPX's web UI into SpxAssetsDir once (staged
// and renamed, so a half-copied tree is never used).
func installSpxAssets(paths pmdir.Paths, spxSrc string) error {
	dest := SpxAssetsDir(paths)
	if fileExists(filepath.Join(dest, "index.html")) {
		return nil
	}
	stage := dest + ".tmp"
	_ = os.RemoveAll(stage)
	if err := copyTree(filepath.Join(spxSrc, "assets", "web-ui"), stage); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("installing SPX's web UI: %w", err)
	}
	_ = os.RemoveAll(dest)
	if err := os.Rename(stage, dest); err != nil {
		os.RemoveAll(stage)
		if fileExists(filepath.Join(dest, "index.html")) {
			return nil // another install finished first
		}
		return fmt.Errorf("installing SPX's web UI: %w", err)
	}
	return nil
}

// copyTree copies the directory src to dst (regular files and dirs).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target, 0o644)
	})
}

// buildSpx configures the matching php.net source tree (for its
// generated main/php_config.h — nothing of PHP is compiled) and builds
// spx.so with one clang call (spxClangArgs), returning its path.
func buildSpx(ctx context.Context, version, sdk, spxSrc, assetsDir, buildDir string, logFile *os.File, logPath string) (string, error) {
	tarPath := filepath.Join(buildDir, fmt.Sprintf("php-%s.tar.xz", version))
	if err := download.ToFile(ctx, phpDistURL(version), tarPath); err != nil {
		return "", err
	}
	if err := runLogged(ctx, buildDir, logFile, "tar", "-xf", tarPath, "-C", buildDir); err != nil {
		return "", sqlsrvBuildError("SPX", err, logPath)
	}
	os.Remove(tarPath)
	phpSrc := filepath.Join(buildDir, "php-"+version)
	if _, err := os.Stat(phpSrc); err != nil {
		return "", sqlsrvBuildError("SPX", fmt.Errorf("expected source directory %s after extracting: %w", phpSrc, err), logPath)
	}
	if err := runLogged(ctx, phpSrc, logFile, "./configure",
		"--disable-all", "--disable-cgi", "--disable-phpdbg", "--without-pear"); err != nil {
		return "", sqlsrvBuildError("SPX", err, logPath)
	}

	configDir := filepath.Join(buildDir, "spx-config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.h"), []byte(spxConfigH(assetsDir)), 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(buildDir, "spx.so")
	if err := runLogged(ctx, spxSrc, logFile, "clang", spxClangArgs(phpSrc, spxSrc, configDir, sdk, out)...); err != nil {
		return "", sqlsrvBuildError("SPX", err, logPath)
	}
	if !fileExists(out) {
		return "", sqlsrvBuildError("SPX", fmt.Errorf("build finished but %s is missing", out), logPath)
	}
	return out, nil
}
