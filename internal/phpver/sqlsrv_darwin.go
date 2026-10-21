//go:build darwin

package phpver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"pm/internal/download"
	"pm/internal/pmdir"
)

// ErrSQLServerToolsMissing is returned by EnsureSQLServer when the
// Command Line Tools (clang, make, the macOS SDK's iconv) aren't
// available to build FreeTDS and pdo_dblib from source.
var ErrSQLServerToolsMissing = errors.New("the Xcode Command Line Tools are required to add SQL Server support (run: xcode-select --install, then: mullion php ext get sqlsrv)")

// pdoDblibIniComment heads the php.ini line EnsureSQLServer adds. It
// must not contain "=" (parseExtLine would read it as a directive).
const pdoDblibIniComment = "SQL Server (pdo_dblib) — built against php.net sources + FreeTDS " + freetdsVersion + " (static builds ship without it)"

// sqlServerRuntimeReady: on macOS pdo_dblib needs nothing outside PHP.
func sqlServerRuntimeReady() bool { return true }

// EnsureSQLServer makes pdo_dblib (SQL Server via FreeTDS) available and
// enabled in the PHP version installed at paths.PhpVersionDir(version).
// The static-php.dev builds ship without it, so it is built from the
// matching php.net source tarball, statically linked against a FreeTDS
// built once from its release tarball and cached under ~/.mullion/lib.
//
// It is a no-op if php -m already lists pdo_dblib, and only re-enables
// the php.ini line if a previously built ext/pdo_dblib.so was disabled.
// Like EnsureLdap it installs nothing system-wide: it only runs xcrun,
// clang/make (via configure/make), tar, and mullion's download helper.
func EnsureSQLServer(ctx context.Context, paths pmdir.Paths, version string) error {
	verDir := paths.PhpVersionDir(version)
	php := filepath.Join(verDir, PhpExeName)
	iniPath := filepath.Join(verDir, "php.ini")
	destSo := filepath.Join(verDir, "ext", "pdo_dblib.so")

	has, err := phpHasModule(php, "pdo_dblib")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := os.Stat(destSo); err == nil {
		if err := verifySharedExt(php, destSo, "pdo_dblib"); err == nil {
			return setSharedExtLine(iniPath, destSo, pdoDblibIniComment, true)
		}
		// A stale or broken build: fall through and rebuild it.
	}

	if err := preflightSQLServerTools(); err != nil {
		return err
	}

	logPath := filepath.Join(paths.LogsDir(), fmt.Sprintf("php-%s-pdo_dblib-build.log", version))
	if err := os.MkdirAll(paths.LogsDir(), 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("creating SQL Server build log %s: %w", logPath, err)
	}
	defer logFile.Close()

	freetds, err := ensureFreeTDS(ctx, paths, logFile, logPath)
	if err != nil {
		return err
	}

	fmt.Printf("Adding SQL Server (pdo_dblib) to PHP %s (builds it from php.net sources, ~1 min)...\n", version)
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp(paths.TmpDir(), "pdo_dblib-build-*")
	if err != nil {
		return fmt.Errorf("creating pdo_dblib build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	soPath, err := buildPdoDblib(ctx, paths, version, freetds, buildDir, logFile, logPath)
	if err != nil {
		return err
	}
	if err := copyFileMode(soPath, destSo, 0o755); err != nil {
		return fmt.Errorf("installing %s: %w", destSo, err)
	}
	if err := verifySharedExt(php, destSo, "pdo_dblib"); err != nil {
		os.Remove(destSo)
		return err
	}
	if err := setSharedExtLine(iniPath, destSo, pdoDblibIniComment, true); err != nil {
		return fmt.Errorf("enabling pdo_dblib.so in %s: %w", iniPath, err)
	}
	return nil
}

// phpHasModule reports whether php -m lists module (case-insensitive).
func phpHasModule(php, module string) (bool, error) {
	out, err := exec.Command(php, iniArgs(php, "-m")...).Output()
	if err != nil {
		return false, fmt.Errorf("running %s -m: %w", php, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), module) {
			return true, nil
		}
	}
	return false, nil
}

// verifySharedExt checks that php can load the module at so and that it
// registers as module.
func verifySharedExt(php, so, module string) error {
	verify := exec.Command(php, "-n", "-d", "extension="+so, "-r",
		fmt.Sprintf(`exit(extension_loaded(%q) ? 0 : 1);`, module))
	if out, err := verify.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("built %s failed to load in %s: %w: %s", filepath.Base(so), php, err, msg)
		}
		return fmt.Errorf("built %s failed to load in %s: %w", filepath.Base(so), php, err)
	}
	return nil
}

// preflightSQLServerTools checks for clang, make, tar, xcrun and the
// SDK's iconv (FreeTDS links the system libiconv) without installing
// anything.
func preflightSQLServerTools() error {
	for _, tool := range []string{"clang", "make", "tar", "xcrun"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%w (%s not found)", ErrSQLServerToolsMissing, tool)
		}
	}
	out, err := exec.Command("xcrun", "--show-sdk-path").Output()
	if err != nil {
		return fmt.Errorf("%w (xcrun --show-sdk-path failed: %v)", ErrSQLServerToolsMissing, err)
	}
	sdk := strings.TrimSpace(string(out))
	if sdk == "" {
		return fmt.Errorf("%w (xcrun --show-sdk-path returned nothing)", ErrSQLServerToolsMissing)
	}
	for _, rel := range []string{
		filepath.Join("usr", "include", "iconv.h"),
		filepath.Join("usr", "lib", "libiconv.tbd"),
	} {
		if _, err := os.Stat(filepath.Join(sdk, rel)); err != nil {
			return fmt.Errorf("%w (missing %s)", ErrSQLServerToolsMissing, filepath.Join(sdk, rel))
		}
	}
	return nil
}

// freetdsReady reports whether dir holds a complete FreeTDS build.
func freetdsReady(dir string) bool {
	for _, rel := range []string{
		filepath.Join("include", "sybdb.h"),
		filepath.Join("lib", "libsybdb.a"),
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			return false
		}
	}
	return true
}

// ensureFreeTDS returns the FreeTDS prefix for this CPU, building it
// from the pinned, checksum-verified release tarball the first time.
// The build is installed with DESTDIR into a staging tree and renamed
// into place, so a half-finished build is never mistaken for a cached
// one and the prefix compiled into it (for freetds.conf) is the final
// one.
func ensureFreeTDS(ctx context.Context, paths pmdir.Paths, logFile *os.File, logPath string) (string, error) {
	prefix := freetdsCacheDir(paths, runtime.GOARCH)
	if freetdsReady(prefix) {
		return prefix, nil
	}
	fmt.Printf("Building FreeTDS %s for SQL Server support (one time, ~1 min)...\n", freetdsVersion)

	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(prefix), err)
	}
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return "", err
	}
	buildDir, err := os.MkdirTemp(paths.TmpDir(), "freetds-build-*")
	if err != nil {
		return "", fmt.Errorf("creating FreeTDS build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	tarPath := filepath.Join(buildDir, "freetds-"+freetdsVersion+".tar.gz")
	if err := download.ToFile(ctx, freetdsURL, tarPath); err != nil {
		return "", err
	}
	if err := verifySHA256(tarPath, freetdsSHA256); err != nil {
		return "", err
	}
	if err := runLogged(ctx, buildDir, logFile, "tar", "-xzf", tarPath, "-C", buildDir); err != nil {
		return "", sqlsrvBuildError("FreeTDS", err, logPath)
	}
	srcDir := filepath.Join(buildDir, "freetds-"+freetdsVersion)
	if err := runLogged(ctx, srcDir, logFile, "./configure", freetdsConfigureArgs(prefix)...); err != nil {
		return "", sqlsrvBuildError("FreeTDS", err, logPath)
	}
	if err := runLogged(ctx, srcDir, logFile, "make", fmt.Sprintf("-j%d", runtime.NumCPU())); err != nil {
		return "", sqlsrvBuildError("FreeTDS", err, logPath)
	}
	stage := filepath.Join(buildDir, "stage")
	if err := runLogged(ctx, srcDir, logFile, "make", "install", "DESTDIR="+stage); err != nil {
		return "", sqlsrvBuildError("FreeTDS", err, logPath)
	}
	staged := filepath.Join(stage, prefix)
	if !freetdsReady(staged) {
		return "", sqlsrvBuildError("FreeTDS", fmt.Errorf("make install produced no libsybdb.a/sybdb.h under %s", staged), logPath)
	}
	if err := os.MkdirAll(filepath.Join(staged, "etc"), 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(staged, "etc", "freetds.conf"), []byte(freetdsConf), 0o644)
	}
	_ = os.RemoveAll(filepath.Join(staged, "share")) // docs and man pages

	// Another install may have finished first; otherwise clear any
	// leftover incomplete build before moving ours into place.
	if freetdsReady(prefix) {
		return prefix, nil
	}
	_ = os.RemoveAll(prefix)
	if err := os.Rename(staged, prefix); err != nil {
		if freetdsReady(prefix) { // another install finished first
			return prefix, nil
		}
		return "", fmt.Errorf("moving FreeTDS into %s: %w", prefix, err)
	}
	return prefix, nil
}

// buildPdoDblib downloads the matching php.net source tarball and builds
// just ext/pdo_dblib.la against freetds, returning the built .so path.
// php's release tarballs ship a generated configure, so neither phpize
// nor autoconf is needed (same approach as buildLdapExtension).
func buildPdoDblib(ctx context.Context, paths pmdir.Paths, version, freetds, buildDir string, logFile *os.File, logPath string) (string, error) {
	tarPath := filepath.Join(buildDir, fmt.Sprintf("php-%s.tar.xz", version))
	if err := download.ToFile(ctx, phpDistURL(version), tarPath); err != nil {
		return "", err
	}
	if err := runLogged(ctx, buildDir, logFile, "tar", "-xf", tarPath, "-C", buildDir); err != nil {
		return "", sqlsrvBuildError("pdo_dblib", err, logPath)
	}
	os.Remove(tarPath)
	srcDir := filepath.Join(buildDir, "php-"+version)
	if _, err := os.Stat(srcDir); err != nil {
		return "", sqlsrvBuildError("pdo_dblib", fmt.Errorf("expected source directory %s after extracting: %w", srcDir, err), logPath)
	}

	configure := exec.CommandContext(ctx, "./configure", pdoDblibConfigureArgs(freetds)...)
	configure.Dir = srcDir
	configure.Env = append(os.Environ(), pdoDblibConfigureEnv()...)
	configure.Stdout, configure.Stderr = logFile, logFile
	fmt.Fprintf(logFile, "\n$ %s ./configure %s\n", strings.Join(pdoDblibConfigureEnv(), " "), strings.Join(pdoDblibConfigureArgs(freetds), " "))
	if err := configure.Run(); err != nil {
		return "", sqlsrvBuildError("pdo_dblib", fmt.Errorf("./configure: %w", err), logPath)
	}
	if err := runLogged(ctx, srcDir, logFile, "make", fmt.Sprintf("-j%d", runtime.NumCPU()), "ext/pdo_dblib/pdo_dblib.la"); err != nil {
		return "", sqlsrvBuildError("pdo_dblib", err, logPath)
	}
	so := filepath.Join(srcDir, "ext", "pdo_dblib", ".libs", "pdo_dblib.so")
	if _, err := os.Stat(so); err != nil {
		return "", sqlsrvBuildError("pdo_dblib", fmt.Errorf("build finished but %s is missing: %w", so, err), logPath)
	}
	return so, nil
}

// sqlsrvBuildError wraps a build-step failure with the log path and its
// last ~15 lines.
func sqlsrvBuildError(what string, cause error, logPath string) error {
	if tail := readLogTail(logPath, 15); tail != "" {
		return fmt.Errorf("building %s: %w (see %s)\n%s", what, cause, logPath, tail)
	}
	return fmt.Errorf("building %s: %w (see %s)", what, cause, logPath)
}

// verifySHA256 checks the file at path against a hex SHA-256 digest.
func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: checksum mismatch (got sha256 %s, want %s)", filepath.Base(path), got, want)
	}
	return nil
}

// copyFileMode copies src to dst (creating dst's directory) with mode.
// It writes a temp file and renames it over dst, so a module php already
// mapped is replaced by a new inode instead of rewritten in place (which
// macOS punishes with code-signature kills).
func copyFileMode(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
