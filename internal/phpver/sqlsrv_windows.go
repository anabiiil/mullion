//go:build windows

package phpver

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/download"
	"pm/internal/elevate"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

// errODBCNeedsAdmin is returned by ensureODBCDriver when the driver is
// missing, the process isn't elevated, and prompting wasn't allowed.
var errODBCNeedsAdmin = errors.New("the Microsoft ODBC Driver 18 for SQL Server is not installed (needs administrator rights; run: mullion php ext get sqlsrv)")

// sqlServerRuntimeReady: sqlsrv/pdo_sqlsrv load without it, but can't
// connect until a Microsoft ODBC Driver for SQL Server is installed.
func sqlServerRuntimeReady() bool {
	_, ok := odbcDriverInstalled()
	return ok
}

// EnsureSQLServer installs Microsoft's sqlsrv and pdo_sqlsrv extensions
// into the PHP version's ext directory, enables them in its php.ini,
// and installs the Microsoft ODBC Driver 18 for SQL Server if no
// suitable ODBC driver is present — asking for UAC approval when not
// already elevated. This is the explicit `mullion php ext get sqlsrv`
// path; installs use the non-prompting ensureSQLServer.
func EnsureSQLServer(ctx context.Context, paths pmdir.Paths, version string) error {
	return ensureSQLServer(ctx, paths, version, true)
}

// ensureSQLServer is EnsureSQLServer with the UAC prompt optional: with
// allowPrompt false a missing ODBC driver is only installed when the
// process is already elevated, and otherwise reported as
// errODBCNeedsAdmin after the extensions are in place.
func ensureSQLServer(ctx context.Context, paths pmdir.Paths, version string, allowPrompt bool) error {
	extDir := filepath.Join(paths.PhpVersionDir(version), "ext")
	if !fileExists(filepath.Join(extDir, "php_sqlsrv.dll")) || !fileExists(filepath.Join(extDir, "php_pdo_sqlsrv.dll")) {
		if err := installMsphpsql(ctx, paths, version); err != nil {
			return err
		}
	}
	for _, name := range []string{"sqlsrv", "pdo_sqlsrv"} {
		if err := SetExtension(paths, version, name, true); err != nil {
			return err
		}
	}
	return ensureODBCDriver(ctx, paths, allowPrompt)
}

// installMsphpsql places php_sqlsrv.dll and php_pdo_sqlsrv.dll matching
// the installed PHP's branch and TS/NTS flavor into its ext directory:
// from the newest stable github.com/microsoft/msphpsql release, or, if
// that doesn't cover the branch (or GitHub is unreachable), from the
// PECL Windows builds that php.net makes of the same extensions.
func installMsphpsql(ctx context.Context, paths pmdir.Paths, version string) error {
	target, err := detectABI(paths, version)
	if err != nil {
		return err
	}
	extDir := filepath.Join(paths.PhpVersionDir(version), "ext")

	ghErr := installMsphpsqlFromGitHub(ctx, paths, target, extDir)
	if ghErr == nil {
		return nil
	}
	fmt.Printf("note: %v; trying the PECL Windows builds\n", ghErr)
	for _, name := range []string{"sqlsrv", "pdo_sqlsrv"} {
		if fileExists(filepath.Join(extDir, "php_"+name+".dll")) {
			continue
		}
		if err := installPeclZip(ctx, paths, version, name); err != nil {
			return fmt.Errorf("Microsoft's SQL Server drivers (msphpsql) don't support PHP %s %s x64 yet, or couldn't be downloaded: %v; PECL: %v", target.Branch, tsLabel(target.TS), ghErr, err)
		}
	}
	return nil
}

func tsLabel(ts bool) string {
	if ts {
		return "TS"
	}
	return "NTS"
}

// installMsphpsqlFromGitHub downloads the newest stable msphpsql Windows
// zip and extracts the two DLLs for target into extDir, renamed to the
// php_<name>.dll form ListExtensions / SetExtension expect.
func installMsphpsqlFromGitHub(ctx context.Context, paths pmdir.Paths, target abi, extDir string) error {
	releases, err := fetchMsphpsqlReleases(ctx)
	if err != nil {
		return fmt.Errorf("listing msphpsql releases: %w", err)
	}
	tag, asset, ok := pickMsphpsqlAsset(releases, target.Branch)
	if !ok {
		return fmt.Errorf("no msphpsql release has a Windows build")
	}
	zipPath := filepath.Join(paths.TmpDir(), "msphpsql-"+asset.Name)
	if err := download.ToFile(ctx, asset.URL, zipPath); err != nil {
		return err
	}
	defer os.Remove(zipPath)

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", zipPath, err)
	}
	defer zr.Close()

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	for _, ext := range []string{"sqlsrv", "pdo_sqlsrv"} {
		var src *zip.File
		for _, f := range zr.File {
			if matchMsphpsqlDLL(f.Name, ext, target.Branch, target.TS) {
				src = f
				break
			}
		}
		if src == nil {
			supported := strings.Join(msphpsqlBranches(names, ext, target.TS), ", ")
			return fmt.Errorf("msphpsql %s has no %s for PHP %s %s x64 (it covers PHP %s)", tag, ext, target.Branch, tsLabel(target.TS), supported)
		}
		if err := extractZipFile(src, filepath.Join(extDir, "php_"+ext+".dll")); err != nil {
			return err
		}
	}
	fmt.Printf("Installed Microsoft SQL Server drivers %s for PHP %s (%s).\n", tag, target.Branch, tsLabel(target.TS))
	return nil
}

func fetchMsphpsqlReleases(ctx context.Context) ([]ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, msphpsqlReleasesAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %s", msphpsqlReleasesAPI, resp.Status)
	}
	var out []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func extractZipFile(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return out.Close()
}

// odbcDriverInstalled returns the name of the first installed driver in
// odbcDriverNames whose registry entry points at an existing DLL.
func odbcDriverInstalled() (string, bool) {
	for _, name := range odbcDriverNames {
		out, err := proc.Quiet("reg", "query", odbcDriverRegKey(name), "/v", "Driver").Output()
		if err != nil {
			continue
		}
		if dll, ok := parseRegSZ(string(out), "Driver"); ok && fileExists(os.ExpandEnv(dll)) {
			return name, true
		}
	}
	return "", false
}

// ensureODBCDriver installs the Microsoft ODBC Driver 18 for SQL Server
// unattended unless a suitable driver is already registered. Elevated,
// it runs msiexec directly; otherwise, only if allowPrompt, through UAC.
func ensureODBCDriver(ctx context.Context, paths pmdir.Paths, allowPrompt bool) error {
	if _, ok := odbcDriverInstalled(); ok {
		return nil
	}
	elevated := elevate.IsElevated()
	if !elevated && !allowPrompt {
		return errODBCNeedsAdmin
	}

	fmt.Println("Downloading the Microsoft ODBC Driver 18 for SQL Server...")
	msiPath := filepath.Join(paths.TmpDir(), "msodbcsql18.msi")
	if err := download.ToFile(ctx, odbcMsiURL, msiPath); err != nil {
		return err
	}
	defer os.Remove(msiPath)
	logPath := filepath.Join(paths.LogsDir(), "msodbcsql18-install.log")
	args := odbcMsiArgs(msiPath, logPath)

	var runErr error
	if elevated {
		cmd := exec.CommandContext(ctx, "msiexec", args...)
		runErr = cmd.Run()
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && msiExitOK(exitErr.ExitCode()) {
			runErr = nil
		}
	} else {
		fmt.Println("Installing it needs administrator rights — approve the Windows prompt.")
		runErr = elevate.Relaunch("msiexec", args...)
	}
	// Relaunch reports 3010 (reboot required) as a failure; the registry
	// is the real answer either way.
	if _, ok := odbcDriverInstalled(); ok {
		return nil
	}
	if runErr != nil {
		return fmt.Errorf("installing the Microsoft ODBC Driver 18 for SQL Server: %w (log: %s)", runErr, logPath)
	}
	return fmt.Errorf("the Microsoft ODBC Driver 18 for SQL Server installer finished but the driver isn't registered (log: %s)", logPath)
}
