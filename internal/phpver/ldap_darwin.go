//go:build darwin

package phpver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"pm/internal/download"
	"pm/internal/pmdir"
)

// ErrLdapToolsMissing is returned by EnsureLdap when the Command Line
// Tools (clang, make, and the macOS SDK's LDAP headers/stub library)
// aren't available to build ldap.so from source.
var ErrLdapToolsMissing = errors.New("the Xcode Command Line Tools are required to add LDAP (run: xcode-select --install)")

// phpDistURL is the php.net source distribution mullion downloads to
// build ext/ldap from, for a given PHP version.
func phpDistURL(version string) string {
	return "https://www.php.net/distributions/php-" + version + ".tar.xz"
}

// EnsureLdap makes the "ldap" extension available to the PHP version
// already installed at paths.PhpVersionDir(version), building it from
// php.net sources against the macOS LDAP.framework — the static-php.dev
// builds mullion installs on macOS ship without ldap.
//
// It is a no-op if the version's php binary already lists ldap (built
// in, or already wired up from a previous run). It never installs any
// software beyond that one shared extension: it only invokes xcrun,
// clang/make (via configure/make), tar, and mullion's own download
// helper.
func EnsureLdap(ctx context.Context, paths pmdir.Paths, version string) error {
	verDir := paths.PhpVersionDir(version)
	php := filepath.Join(verDir, PhpExeName)

	has, err := phpHasLdap(php)
	if err != nil {
		return err
	}
	if has {
		return nil
	}

	sdk, err := preflightLdapTools()
	if err != nil {
		return err
	}

	fmt.Printf("Adding LDAP to PHP %s (builds ldap.so from php.net sources, ~1-2 min)...\n", version)

	buildDir, err := os.MkdirTemp(paths.TmpDir(), "ldap-build-*")
	if err != nil {
		return fmt.Errorf("creating LDAP build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	logPath := filepath.Join(paths.LogsDir(), fmt.Sprintf("php-%s-ldap-build.log", version))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("creating LDAP build log %s: %w", logPath, err)
	}
	defer logFile.Close()

	soPath, err := buildLdapExtension(ctx, paths, version, sdk, buildDir, logFile, logPath)
	if err != nil {
		return err
	}

	destSo := filepath.Join(verDir, "ext", "ldap.so")
	if err := installLdapSo(php, soPath, destSo); err != nil {
		return err
	}

	iniPath := filepath.Join(verDir, "php.ini")
	if err := appendLdapIni(iniPath, version, destSo); err != nil {
		return fmt.Errorf("enabling ldap.so in %s: %w", iniPath, err)
	}
	return nil
}

// phpHasLdap reports whether php -m already lists the ldap extension.
func phpHasLdap(php string) (bool, error) {
	out, err := exec.Command(php, "-m").Output()
	if err != nil {
		return false, fmt.Errorf("running %s -m: %w", php, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "ldap") {
			return true, nil
		}
	}
	return false, nil
}

// ldapSDK holds the paths preflightLdapTools resolved out of the active
// macOS SDK, reused by buildLdapExtension.
type ldapSDK struct {
	path string // xcrun --show-sdk-path
}

// preflightLdapTools checks that everything EnsureLdap needs to build
// ext/ldap is present — the Command Line Tools' SDK (with its LDAP
// header and stub library) plus clang and make — without installing
// anything. It returns ErrLdapToolsMissing (wrapped with the specific
// reason) if not.
func preflightLdapTools() (ldapSDK, error) {
	for _, tool := range []string{"clang", "make", "tar", "xcrun"} {
		if _, err := exec.LookPath(tool); err != nil {
			return ldapSDK{}, fmt.Errorf("%w (%s not found)", ErrLdapToolsMissing, tool)
		}
	}
	out, err := exec.Command("xcrun", "--show-sdk-path").Output()
	if err != nil {
		return ldapSDK{}, fmt.Errorf("%w (xcrun --show-sdk-path failed: %v)", ErrLdapToolsMissing, err)
	}
	sdk := strings.TrimSpace(string(out))
	if sdk == "" {
		return ldapSDK{}, fmt.Errorf("%w (xcrun --show-sdk-path returned nothing)", ErrLdapToolsMissing)
	}
	for _, rel := range []string{
		filepath.Join("usr", "lib", "libldap.tbd"),
		filepath.Join("usr", "include", "ldap.h"),
	} {
		if _, err := os.Stat(filepath.Join(sdk, rel)); err != nil {
			return ldapSDK{}, fmt.Errorf("%w (missing %s)", ErrLdapToolsMissing, filepath.Join(sdk, rel))
		}
	}
	return ldapSDK{path: sdk}, nil
}

// buildLdapExtension downloads the matching php.net source tarball,
// stages a fake LDAP "prefix" that points configure at the macOS SDK's
// real LDAP headers and stub libraries, and builds just ext/ldap.la.
// configure/make output goes to logFile; on failure the last lines are
// read back from logPath and folded into the returned error. It returns
// the path to the built ext/ldap/.libs/ldap.so.
func buildLdapExtension(ctx context.Context, paths pmdir.Paths, version string, sdk ldapSDK, buildDir string, logFile *os.File, logPath string) (string, error) {
	prefix := filepath.Join(buildDir, "prefix")
	if err := os.MkdirAll(filepath.Join(prefix, "lib"), 0o755); err != nil {
		return "", fmt.Errorf("preparing LDAP build prefix: %w", err)
	}
	if err := os.Symlink(filepath.Join(sdk.path, "usr", "include"), filepath.Join(prefix, "include")); err != nil {
		return "", fmt.Errorf("linking SDK headers into LDAP build prefix: %w", err)
	}
	for _, lib := range []string{"ldap", "lber", "ldap_r"} {
		tbdSrc := filepath.Join(sdk.path, "usr", "lib", "lib"+lib+".tbd")
		if err := os.Symlink(tbdSrc, filepath.Join(prefix, "lib", "lib"+lib+".tbd")); err != nil {
			return "", fmt.Errorf("linking %s into LDAP build prefix: %w", tbdSrc, err)
		}
		// configure only checks that the .dylib exists; the linker
		// prefers the real .tbd stub alongside it.
		if err := os.WriteFile(filepath.Join(prefix, "lib", "lib"+lib+".dylib"), nil, 0o644); err != nil {
			return "", fmt.Errorf("staging lib%s.dylib placeholder: %w", lib, err)
		}
	}

	tarPath := filepath.Join(paths.TmpDir(), fmt.Sprintf("php-%s.tar.xz", version))
	if err := download.ToFile(ctx, phpDistURL(version), tarPath); err != nil {
		return "", err
	}
	defer os.Remove(tarPath)

	// /usr/bin/tar on macOS is bsdtar, which extracts .xz directly.
	if err := runLogged(ctx, buildDir, logFile, "tar", "-xf", tarPath, "-C", buildDir); err != nil {
		return "", ldapBuildError(err, logPath)
	}
	srcDir := filepath.Join(buildDir, "php-"+version)
	if _, err := os.Stat(srcDir); err != nil {
		return "", ldapBuildError(fmt.Errorf("expected source directory %s after extracting: %w", srcDir, err), logPath)
	}

	if err := runLogged(ctx, srcDir, logFile, "./configure",
		"--disable-all", "--disable-cgi", "--disable-phpdbg", "--without-pear",
		"--with-ldap=shared,"+prefix); err != nil {
		return "", ldapBuildError(err, logPath)
	}
	if err := runLogged(ctx, srcDir, logFile, "make", fmt.Sprintf("-j%d", runtime.NumCPU()), "ext/ldap/ldap.la"); err != nil {
		return "", ldapBuildError(err, logPath)
	}

	so := filepath.Join(srcDir, "ext", "ldap", ".libs", "ldap.so")
	if _, err := os.Stat(so); err != nil {
		return "", ldapBuildError(fmt.Errorf("build finished but %s is missing: %w", so, err), logPath)
	}
	return so, nil
}

// runLogged runs name(args...) in dir with stdout/stderr sent to log
// rather than the terminal.
func runLogged(ctx context.Context, dir string, log *os.File, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	fmt.Fprintf(log, "\n$ %s %s\n", name, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// ldapBuildError wraps a build-step failure with the log path and its
// last ~15 lines, so the caller doesn't have to go spelunking.
func ldapBuildError(cause error, logPath string) error {
	tail := readLogTail(logPath, 15)
	if tail == "" {
		return fmt.Errorf("building LDAP extension: %w (see %s)", cause, logPath)
	}
	return fmt.Errorf("building LDAP extension: %w (see %s)\n%s", cause, logPath, tail)
}

// readLogTail returns the last n lines of the file at path, or "" if it
// can't be read.
func readLogTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// installLdapSo copies the freshly built ldap.so into verDir/ext and
// verifies php can actually load it, deleting the copy again if not.
func installLdapSo(php, builtSo, destSo string) error {
	if err := os.MkdirAll(filepath.Dir(destSo), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(destSo), err)
	}
	data, err := os.ReadFile(builtSo)
	if err != nil {
		return fmt.Errorf("reading built ldap.so: %w", err)
	}
	if err := os.WriteFile(destSo, data, 0o755); err != nil {
		return fmt.Errorf("installing %s: %w", destSo, err)
	}

	verify := exec.Command(php, "-d", "extension="+destSo, "-r",
		`exit(extension_loaded("ldap") ? 0 : 1);`)
	if out, err := verify.CombinedOutput(); err != nil {
		os.Remove(destSo)
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("built ldap.so failed to load in %s: %w: %s", php, err, msg)
		}
		return fmt.Errorf("built ldap.so failed to load in %s: %w", php, err)
	}
	return nil
}

// ldapIniMarker is the substring that identifies an existing active
// extension line for ldap.so, so appendLdapIni never duplicates it.
const ldapIniMarker = "ldap.so"

// appendLdapIni appends the extension="<destSo>" directive (plus a
// short comment) to iniPath, unless an active line already mentions
// ldap.so. A commented-out line (";extension=...ldap.so") is left in
// place and an active one is appended alongside it. The file's mode is
// preserved.
func appendLdapIni(iniPath, version, destSo string) error {
	mode := os.FileMode(0o644)
	var content string
	if data, err := os.ReadFile(iniPath); err == nil {
		content = string(data)
		if info, statErr := os.Stat(iniPath); statErr == nil {
			mode = info.Mode()
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", iniPath, err)
	}

	for _, line := range strings.Split(content, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, ";") {
			continue
		}
		if strings.HasPrefix(l, "extension=") && strings.Contains(l, ldapIniMarker) {
			return nil // already enabled
		}
	}

	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += fmt.Sprintf(
		"\n; LDAP — built against PHP %s headers + macOS LDAP.framework (static builds ship without it)\nextension=\"%s\"\n",
		version, destSo,
	)
	return os.WriteFile(iniPath, []byte(content), mode)
}
