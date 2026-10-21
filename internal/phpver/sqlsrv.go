package phpver

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"pm/internal/pmdir"
)

// SQL Server support differs per platform, because the two PHP builds
// mullion installs differ:
//
//   - Windows (windows.php.net NTS x64 zips): Microsoft's own sqlsrv and
//     pdo_sqlsrv DLLs (github.com/microsoft/msphpsql), which talk to SQL
//     Server through the Microsoft ODBC Driver 18 for SQL Server.
//   - macOS (static-php.dev builds): pdo_dblib, built from the matching
//     php.net source tarball and statically linked against FreeTDS.
//     Microsoft's sqlsrv would need unixODBC + msodbcsql18, which are only
//     distributed through Microsoft's Homebrew tap. Laravel's
//     SqlServerConnector uses the dblib DSN whenever pdo_sqlsrv is absent,
//     and Adminer's mssql driver falls back to PDO_DBLIB the same way, so
//     DB_CONNECTION=sqlsrv works unchanged.

// SQLServerExtensions returns the extension names, as ListExtensions and
// the panel present them, that give this platform's PHP SQL Server
// support.
func SQLServerExtensions() []string {
	if runtime.GOOS == "windows" {
		return []string{"sqlsrv", "pdo_sqlsrv"}
	}
	return []string{"pdo_dblib"}
}

// IsSQLServerExtension reports whether name (as typed by a user, e.g.
// "sqlsrv", "php_pdo_sqlsrv.dll", "mssql") asks for SQL Server support.
// Every platform accepts every spelling, so `mullion php ext get sqlsrv`
// works on macOS too and builds pdo_dblib there.
func IsSQLServerExtension(name string) bool {
	switch normalizeExtName(name) {
	case "sqlsrv", "pdo_sqlsrv", "pdo_dblib", "dblib", "mssql", "sqlserver":
		return true
	}
	return false
}

// HasSQLServerDriver reports, read-only, whether the installed PHP
// version can talk to SQL Server right now: every extension in
// SQLServerExtensions is present and enabled, and (Windows only) a
// Microsoft ODBC Driver for SQL Server is installed.
func HasSQLServerDriver(paths pmdir.Paths, version string) bool {
	exts, err := ListExtensions(paths, version)
	if err != nil {
		return false
	}
	enabled := map[string]bool{}
	for _, e := range exts {
		if e.Enabled {
			enabled[e.Name] = true
		}
	}
	for _, name := range SQLServerExtensions() {
		if !enabled[name] {
			return false
		}
	}
	return sqlServerRuntimeReady()
}

// fileExists reports whether p exists.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---- Windows: msphpsql DLL selection (pure; tested on every platform) ----

// msphpsqlReleasesAPI lists Microsoft's msphpsql releases, newest first.
const msphpsqlReleasesAPI = "https://api.github.com/repos/microsoft/msphpsql/releases?per_page=10"

// ghRelease / ghAsset are the parts of the GitHub releases API we use.
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// msphpsqlBundleRe matches the one-zip-for-every-PHP Windows asset that
// msphpsql has shipped since 5.12 ("Windows_5.13.3RTW.zip"); the
// per-branch form ("Windows-8.3.zip", 5.11 and earlier) is matched by
// msphpsqlBranchAsset.
var msphpsqlBundleRe = regexp.MustCompile(`^Windows_[0-9][^/]*\.zip$`)

// pickMsphpsqlAsset picks the zip to download for a PHP branch ("8.4")
// from a releases listing (newest first): the newest stable release that
// ships either a per-branch zip for that branch or the all-branches
// bundle. Whether a bundle actually contains the branch is only known
// after downloading it (see matchMsphpsqlDLL).
func pickMsphpsqlAsset(releases []ghRelease, branch string) (tag string, asset ghAsset, ok bool) {
	branchAsset := "Windows-" + branch + ".zip"
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		for _, a := range r.Assets {
			if a.Name == branchAsset {
				return r.TagName, a, true
			}
		}
		for _, a := range r.Assets {
			if msphpsqlBundleRe.MatchString(a.Name) {
				return r.TagName, a, true
			}
		}
	}
	return "", ghAsset{}, false
}

// msphpsqlDLLName is the file name msphpsql gives an extension DLL in its
// release zips, e.g. ("pdo_sqlsrv", "8.4", false) -> php_pdo_sqlsrv_84_nts_x64.dll.
func msphpsqlDLLName(ext, branch string, ts bool) string {
	flavor := "nts"
	if ts {
		flavor = "ts"
	}
	return fmt.Sprintf("php_%s_%s_%s_x64.dll", ext, strings.ReplaceAll(branch, ".", ""), flavor)
}

// matchMsphpsqlDLL reports whether a zip entry is the x64 DLL of ext for
// the given branch and thread-safety flavor. Current zips name it
// "Windows/php_sqlsrv_84_nts_x64.dll"; older per-branch zips put it in an
// x64 folder without the suffix ("x64/php_sqlsrv_83_nts.dll").
func matchMsphpsqlDLL(entry, ext, branch string, ts bool) bool {
	entry = strings.ReplaceAll(entry, `\`, "/")
	base := strings.ToLower(path.Base(entry))
	want := msphpsqlDLLName(ext, branch, ts)
	if base == want {
		return true
	}
	legacy := strings.TrimSuffix(want, "_x64.dll") + ".dll"
	return base == legacy && strings.EqualFold(path.Base(path.Dir(entry)), "x64")
}

// msphpsqlBranches lists the PHP branches (e.g. "8.3", "8.4") a zip's
// entry names provide x64 DLLs of ext for, used in the "not supported
// yet" message.
func msphpsqlBranches(entries []string, ext string, ts bool) []string {
	flavor := "nts"
	if ts {
		flavor = "ts"
	}
	re := regexp.MustCompile(`^php_` + regexp.QuoteMeta(ext) + `_(\d)(\d+)_` + flavor + `(?:_x64)?\.dll$`)
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		m := re.FindStringSubmatch(strings.ToLower(path.Base(strings.ReplaceAll(e, `\`, "/"))))
		if m == nil {
			continue
		}
		b := m[1] + "." + m[2]
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// ---- Windows: Microsoft ODBC Driver detection / install (pure parts) ----

// odbcDriverNames are the ODBC drivers msphpsql 5.12+ works with, most
// preferred first. 18 is what mullion installs; an existing 17 is fine.
var odbcDriverNames = []string{"ODBC Driver 18 for SQL Server", "ODBC Driver 17 for SQL Server"}

// odbcDriverRegKey is the registry key an ODBC driver registers under.
func odbcDriverRegKey(driver string) string {
	return `HKLM\SOFTWARE\ODBC\ODBCINST.INI\` + driver
}

// odbcMsiURL is Microsoft's x64 download link for the current ODBC Driver
// 18 for SQL Server (18.7.1.1 at the time of writing), from
// https://learn.microsoft.com/sql/connect/odbc/download-odbc-driver-for-sql-server
const odbcMsiURL = "https://go.microsoft.com/fwlink/?linkid=2378279&clcid=0x409"

// parseRegSZ extracts a REG_SZ / REG_EXPAND_SZ value from `reg query
// <key> /v <name>` output, which looks like:
//
//	HKEY_LOCAL_MACHINE\SOFTWARE\ODBC\ODBCINST.INI\ODBC Driver 18 for SQL Server
//	    Driver    REG_SZ    C:\Windows\system32\msodbcsql18.dll
func parseRegSZ(out, name string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || !strings.EqualFold(fields[0], name) {
			continue
		}
		if fields[1] != "REG_SZ" && fields[1] != "REG_EXPAND_SZ" {
			continue
		}
		// The value itself may contain spaces ("C:\Program Files\...").
		idx := strings.Index(line, fields[1])
		val := strings.TrimSpace(line[idx+len(fields[1]):])
		if val != "" {
			return val, true
		}
	}
	return "", false
}

// odbcMsiArgs builds the unattended msiexec command line for the ODBC
// driver installer. IACCEPTMSODBCSQLLICENSETERMS=YES is Microsoft's
// documented switch for accepting the driver's license non-interactively.
func odbcMsiArgs(msiPath, logPath string) []string {
	args := []string{"/i", msiPath, "/quiet", "/norestart", "IACCEPTMSODBCSQLLICENSETERMS=YES"}
	if logPath != "" {
		args = append(args, "/l*v", logPath)
	}
	return args
}

// msiExitOK reports whether an msiexec exit code means success:
// 0, or 3010/1641 (succeeded, reboot required/initiated).
func msiExitOK(code int) bool {
	return code == 0 || code == 3010 || code == 1641
}

// ---- macOS: FreeTDS + pdo_dblib build (pure parts) ----

// FreeTDS release mullion builds pdo_dblib against, from
// https://www.freetds.org/files/stable/ (md5 979c67c11779701c9bda7118628d4797
// as published there; sha256 computed from that verified download).
const (
	freetdsVersion = "1.5.19"
	freetdsURL     = "https://www.freetds.org/files/stable/freetds-" + freetdsVersion + ".tar.gz"
	freetdsSHA256  = "ff70d0c8ae8dd23e7b1314274d6e9d30b479998415760b3fa656754b737c2bd6"
)

// freetdsCacheDir is where the built FreeTDS (static libsybdb.a plus its
// headers) is kept, per version and CPU architecture, so every PHP
// version reuses one build.
func freetdsCacheDir(paths pmdir.Paths, arch string) string {
	return filepath.Join(paths.Home, "lib", "freetds-"+freetdsVersion+"-"+arch)
}

// freetdsConfigureArgs is FreeTDS's ./configure command line: only the
// static db-lib/ct-lib libraries, no ODBC driver, apps, server or pool.
//
// No TLS library: static-php's OpenSSL lives inside the php binary with
// no headers to compile against, and the macOS SDK ships neither OpenSSL
// nor GnuTLS headers. Without one FreeTDS answers the TDS pre-login with
// ENCRYPT_NOT_SUP, which SQL Server accepts unless the server has
// "Force Encryption" turned on (it is off by default, including in the
// official mssql/server container images); against a server that forces
// it, FreeTDS fails the login rather than connecting unencrypted.
// iconv comes from the system's /usr/lib/libiconv (SQL Server varchar
// columns are usually CP1252, which FreeTDS's built-in tables lack).
func freetdsConfigureArgs(prefix string) []string {
	return []string{
		"--prefix=" + prefix,
		"--enable-static", "--disable-shared", "--with-pic",
		"--disable-odbc", "--disable-apps", "--disable-server", "--disable-pool",
		"--without-openssl", "--without-gnutls", "--disable-krb5",
		"--with-tdsver=7.4",
	}
}

// pdoDblibConfigureArgs is php-src's ./configure command line for
// building just ext/pdo_dblib as a shared module against the FreeTDS
// installed at freetdsPrefix. PDO itself must be enabled for configure
// to accept pdo_dblib; it's never built or used (the static php has it).
func pdoDblibConfigureArgs(freetdsPrefix string) []string {
	return []string{
		"--disable-all", "--disable-cgi", "--disable-phpdbg", "--without-pear",
		"--enable-pdo", "--with-pdo-dblib=shared," + freetdsPrefix,
	}
}

// pdoDblibConfigureEnv is the extra environment for that configure run:
// libsybdb.a needs iconv, and php-src's pdo_dblib config.m4 only adds
// -lsybdb, so without this the module links with iconv unresolved and
// fails to load into php.
func pdoDblibConfigureEnv() []string {
	return []string{"LDFLAGS=-liconv"}
}

// freetdsConf is the freetds.conf written into the FreeTDS cache (the
// path FreeTDS was configured with). Laravel and Adminer pass
// charset=utf8 in their DSNs, so client charset only sets the default.
const freetdsConf = `; Written by mullion. FreeTDS settings for PHP's pdo_dblib.
; Per-server sections ([name] + host/port) can be added below.
[global]
	tds version = 7.4
	client charset = UTF-8
	; built without TLS: SQL Server must not have Force Encryption on
	encryption = off
`

// ---- shared-module ini lines (macOS pdo_dblib / ldap) ----

// setSharedExtLine enables or disables the shared module soPath in the
// php.ini at iniPath. Enabling uncomments the first commented
// `extension=` line naming the same file, or appends
// `extension="<soPath>"` (after a "; comment" line, if given) when there
// is none; disabling comments out every active line for it. Lines are
// matched by the module's file name, so a path that moved still counts.
// The file's mode is preserved.
func setSharedExtLine(iniPath, soPath, comment string, enable bool) error {
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

	want := normalizeExtName(filepath.Base(soPath))
	lines := strings.Split(content, "\n")
	changed, satisfied := false, false
	for i, line := range lines {
		name, active, _ := parseExtLine(line)
		if name != want {
			continue
		}
		switch {
		case enable && active:
			satisfied = true
		case enable && !satisfied:
			lines[i] = `extension="` + soPath + `"`
			satisfied, changed = true, true
		case !enable && active:
			lines[i] = ";" + strings.TrimRight(line, "\r")
			changed = true
		}
	}
	if enable && !satisfied {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		if comment != "" {
			lines = append(lines, "; "+comment)
		}
		lines = append(lines, `extension="`+soPath+`"`, "")
		changed = true
	}
	if !changed {
		return nil
	}
	return os.WriteFile(iniPath, []byte(strings.Join(lines, "\n")), mode)
}
