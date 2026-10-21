// Package mysql installs and controls a local MySQL server under
// ~/.mullion/mysql, following the same pattern as the PHP versions: binaries
// per version, one detached background process, a pid file, and a port
// probe to know whether it's up.
package mysql

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/sysproc"
	"pm/internal/term"
)

// DefaultPort is MySQL's standard port, which local dev tools expect.
const DefaultPort = 3306

// Port is the port Mullion's server listens on: DefaultPort unless the
// user moved it (config mysqlPort) because something else holds 3306.
// app.ApplyPortConfig sets it from the config at startup; everything
// here — my.ini, the client tools, the readiness probe — reads it at
// call time.
var Port = DefaultPort

// RootPassword is the root password of Mullion's server ("" until the
// user sets one with `mullion mysql password`). app.New loads it from
// the config; every client invocation here honors it.
var RootPassword string

// withPassword hands the root password to a client tool via the
// environment (MYSQL_PWD), keeping it out of `ps` output.
func withPassword(cmd *exec.Cmd) *exec.Cmd {
	if RootPassword != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+RootPassword)
	}
	return cmd
}

// MariaDB installs are versioned as "mariadb-<x.y.z>" so one config
// field and one directory layout cover both flavors.
const mariaPrefix = "mariadb-"

// IsMaria reports whether a stored version string is a MariaDB one.
func IsMaria(version string) bool { return strings.HasPrefix(version, mariaPrefix) }

// Label renders a version for humans: "MySQL 8.4.11" / "MariaDB 11.4.5".
func Label(version string) string {
	if IsMaria(version) {
		return "MariaDB " + strings.TrimPrefix(version, mariaPrefix)
	}
	return "MySQL " + version
}

// binExe finds a tool in the version's bin dir, accepting both the
// MySQL and the MariaDB name for it (with or without .exe).
func binExe(paths pmdir.Paths, version string, names ...string) string {
	for _, n := range names {
		for _, cand := range []string{n, n + ".exe"} {
			p := filepath.Join(paths.MysqlVersionDir(version), "bin", cand)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join(paths.MysqlVersionDir(version), "bin", pmdir.ExeName(names[0]))
}

func serverExe(paths pmdir.Paths, version string) string {
	return binExe(paths, version, "mysqld", "mariadbd")
}

// MariaLatest resolves the newest stable MariaDB via the official REST
// API on downloads.mariadb.org.
func MariaLatest(ctx context.Context) (string, error) {
	body, err := fetchPage(ctx, "https://downloads.mariadb.org/rest-api/mariadb/")
	if err != nil {
		return "", err
	}
	var index struct {
		MajorReleases []struct {
			ID     string `json:"release_id"`
			Status string `json:"release_status"`
		} `json:"major_releases"`
	}
	if err := json.Unmarshal([]byte(body), &index); err != nil {
		return "", fmt.Errorf("parsing MariaDB releases: %w", err)
	}
	best := ""
	for _, r := range index.MajorReleases {
		if !strings.EqualFold(r.Status, "Stable") {
			continue
		}
		if best == "" || compareSeries(r.ID, best) > 0 {
			best = r.ID
		}
	}
	if best == "" {
		return "", fmt.Errorf("no stable MariaDB release found")
	}

	body, err = fetchPage(ctx, "https://downloads.mariadb.org/rest-api/mariadb/"+best+"/")
	if err != nil {
		return "", err
	}
	var series struct {
		Releases map[string]any `json:"releases"`
	}
	if err := json.Unmarshal([]byte(body), &series); err != nil {
		return "", fmt.Errorf("parsing MariaDB %s releases: %w", best, err)
	}
	newest := ""
	for v := range series.Releases {
		if newest == "" || compare(v, newest) > 0 {
			newest = v
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no downloadable MariaDB %s release found", best)
	}
	return mariaPrefix + newest, nil
}

// compareSeries orders two-part branch ids like "11.4".
func compareSeries(a, b string) int {
	return compare(a+".0", b+".0")
}

// ResolveVersionArg turns a CLI/UI version argument into a full stored
// version: "" -> newest MySQL LTS, "latest" -> newest MySQL overall,
// "mariadb" -> newest stable MariaDB, "8.0" -> newest of that MySQL
// branch, full versions pass through.
func ResolveVersionArg(ctx context.Context, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	switch {
	case arg == "":
		return DefaultVersion(ctx)
	case strings.EqualFold(arg, "latest"):
		return Latest(ctx)
	case strings.EqualFold(arg, "mariadb"):
		return MariaLatest(ctx)
	case IsMaria(strings.ToLower(arg)):
		return strings.ToLower(arg), nil
	case strings.Count(arg, ".") == 2:
		return arg, nil
	case strings.Count(arg, ".") == 1:
		return LatestSeries(ctx, arg)
	}
	return "", fmt.Errorf("invalid version %q — use a full version (8.4.11), a branch (8.4), `latest`, `mariadb`, or `mariadb-11.4.5`", arg)
}

// DefaultSeries is what a bare `mullion mysql install` (and setup) gets:
// the LTS branch, which is what most of the ecosystem tests against.
// Innovation releases stay available via `mullion mysql install latest`.
const DefaultSeries = "8.4"

// DefaultVersion resolves the newest release of the LTS series.
func DefaultVersion(ctx context.Context) (string, error) {
	return LatestSeries(ctx, DefaultSeries)
}

// Latest scrapes dev.mysql.com's download page for the newest Windows
// x64 zip release overall (innovation branch included).
func Latest(ctx context.Context) (string, error) {
	body, err := fetchPage(ctx, "https://dev.mysql.com/downloads/mysql/"+downloadPageQuery)
	if err != nil {
		return "", err
	}
	if v := newestOnPage(body, ""); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("could not find a downloadable release on the MySQL download page; pass a version explicitly: mullion mysql install 8.4.11")
}

// LatestSeries resolves the newest release of one branch, e.g. "8.4".
func LatestSeries(ctx context.Context, series string) (string, error) {
	// Each branch has its own download page; fall back to the generic one.
	if body, err := fetchPage(ctx, "https://dev.mysql.com/downloads/mysql/"+series+".html"+downloadPageQuery); err == nil {
		if v := newestOnPage(body, series+"."); v != "" {
			return v, nil
		}
	}
	body, err := fetchPage(ctx, "https://dev.mysql.com/downloads/mysql/"+downloadPageQuery)
	if err != nil {
		return "", err
	}
	if v := newestOnPage(body, series+"."); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("could not find a downloadable MySQL %s release; pass a full version explicitly", series)
}

func fetchPage(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(body), err
}

func newestOnPage(body, prefix string) string {
	best := ""
	for _, m := range versionRe.FindAllStringSubmatch(body, -1) {
		v := m[1] + "." + m[2] + "." + m[3]
		if prefix != "" && !strings.HasPrefix(v, prefix) {
			continue
		}
		if best == "" || compare(v, best) > 0 {
			best = v
		}
	}
	return best
}

// Install downloads and unpacks a release into the versions directory.
// It is a no-op if the version is already installed.
func Install(ctx context.Context, paths pmdir.Paths, version string) error {
	if _, err := os.Stat(serverExe(paths, version)); err == nil {
		return nil
	}
	if err := flavorSupported(version); err != nil {
		return err
	}

	fmt.Printf("Downloading %s...\n", Label(version))
	var lastErr error
	for _, url := range downloadURLs(version) {
		archivePath := filepath.Join(paths.TmpDir(), filepath.Base(url))
		if lastErr = download.ToFile(ctx, url, archivePath); lastErr != nil {
			// Only an HTTP miss means "try the next candidate name".
			// A local failure (disk full, connection died mid-file)
			// would fail them all — surface it immediately instead of
			// re-downloading gigabytes.
			if strings.Contains(lastErr.Error(), "HTTP 4") || strings.Contains(lastErr.Error(), "HTTP 5") {
				continue
			}
			return fmt.Errorf("downloading %s: %w", Label(version), lastErr)
		}
		defer os.Remove(archivePath)

		// The archive wraps everything in a <flavor>-<v>-<platform>/
		// directory: extract to a staging dir, then move that directory
		// into place.
		staging := filepath.Join(paths.TmpDir(), "mysql-extract")
		os.RemoveAll(staging)
		var err error
		if strings.HasSuffix(url, ".zip") {
			err = archive.ExtractZip(archivePath, staging)
		} else {
			err = archive.ExtractTarGz(archivePath, staging)
		}
		if err != nil {
			return fmt.Errorf("extracting %s: %w", archivePath, err)
		}
		inner, err := findServerDir(staging)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(paths.MysqlDir(), 0o755); err != nil {
			return err
		}
		if err := os.Rename(inner, paths.MysqlVersionDir(version)); err != nil {
			return err
		}
		os.RemoveAll(staging)
		return nil
	}
	return fmt.Errorf("downloading %s: %w", Label(version), lastErr)
}

// findServerDir locates the extracted directory that holds bin/mysqld
// (or bin/mariadbd) — more robust than predicting the archive's
// top-level directory name.
func findServerDir(staging string) (string, error) {
	candidates := []string{staging}
	if entries, err := os.ReadDir(staging); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(staging, e.Name()))
			}
		}
	}
	for _, dir := range candidates {
		for _, name := range []string{"mysqld", "mysqld.exe", "mariadbd", "mariadbd.exe"} {
			if _, err := os.Stat(filepath.Join(dir, "bin", name)); err == nil {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("unexpected archive layout: no bin/mysqld under %s", staging)
}

// EnsureInitialized creates the data directory (root user, no password)
// on first run and writes my.ini.
func EnsureInitialized(paths pmdir.Paths, version string) error {
	if err := writeIni(paths, version); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(paths.MysqlDataDir(), "mysql")); err == nil {
		return nil
	}
	fmt.Printf("Initializing the %s data directory...\n", Label(version))
	var cmd *exec.Cmd
	if IsMaria(version) {
		cmd = proc.Quiet(binExe(paths, version, "mariadb-install-db", "mysql_install_db"),
			"--datadir="+paths.MysqlDataDir())
	} else {
		cmd = proc.Quiet(serverExe(paths, version),
			"--no-defaults", "--initialize-insecure",
			"--basedir="+paths.MysqlVersionDir(version),
			"--datadir="+paths.MysqlDataDir())
	}
	cmd.Dir = paths.MysqlVersionDir(version)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("initializing data directory: %v: %s", err, out)
	}
	return nil
}

// Start launches mysqld detached and waits for the port to come up.
func Start(paths pmdir.Paths, version string) error {
	if Running() {
		return nil
	}
	// A server that was just told to stop (by another command, or a crash
	// still unwinding) keeps the data files locked until it exits.
	waitExited(paths, version, 30*time.Second)
	// Keep my.ini in step with Port even when the caller skipped
	// EnsureInitialized (the port may have changed since it last ran).
	if err := writeIni(paths, version); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(paths.LogsDir(), "mysql.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	// --no-monitor: without it, Windows mysqld is a supervisor process
	// whose real server runs as a child in a kill-on-close job object —
	// the supervisor dying (e.g. with the setup console window) takes
	// the server down, and the pid we record wouldn't be the server's.
	// MariaDB has no monitor process (and no such flag).
	args := []string{"--defaults-file=" + paths.MysqlIni()}
	if runtime.GOOS == "windows" && !IsMaria(version) {
		args = append(args, "--no-monitor")
	}
	cmd := proc.Quiet(serverExe(paths, version), args...)
	cmd.Dir = paths.MysqlVersionDir(version)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting mysqld: %w", err)
	}
	pid := cmd.Process.Pid
	// Detach: the process must outlive us.
	_ = cmd.Process.Release()
	if err := os.WriteFile(pidFile(paths), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}

	for i := 0; i < 120; i++ {
		if Running() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("mysqld did not start listening on port %d (see %s)",
		Port, filepath.Join(paths.LogsDir(), "mysql.err"))
}

// Stop shuts the server down cleanly via mysqladmin, falling back to
// killing the recorded pid, and waits until the server has actually
// exited — the shutdown command returns while mysqld is still flushing
// and holding its data files.
func Stop(paths pmdir.Paths, version string) error {
	defer os.Remove(pidFile(paths))
	if !Running() {
		killByPidFile(paths)
		return nil
	}
	admin := binExe(paths, version, "mysqladmin", "mariadb-admin")
	cmd := withPassword(proc.Quiet(admin, "--user=root", "--host=127.0.0.1",
		fmt.Sprintf("--port=%d", Port), "shutdown"))
	if out, err := cmd.CombinedOutput(); err != nil {
		killByPidFile(paths)
		return fmt.Errorf("mysqladmin shutdown: %v: %s (killed the process instead)", err, out)
	}
	for i := 0; i < 120 && Running(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	// The port closes first; InnoDB then flushes for up to a second or so
	// while still holding ibdata1. A restart that starts the next mysqld
	// in that window fails with "ibdata1 must be writable".
	waitExited(paths, version, 60*time.Second)
	return nil
}

// waitExited waits until no server process runs from the version's
// folder (mysqld, or mariadbd for MariaDB), or the timeout passes.
func waitExited(paths pmdir.Paths, version string, timeout time.Duration) {
	dir := paths.MysqlVersionDir(version)
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); {
		if len(sysproc.ProcessesUnder(dir, "")) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Running reports whether something is serving the MySQL port.
func Running() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", Port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func pidFile(paths pmdir.Paths) string {
	return filepath.Join(paths.PidsDir(), "mysql.pid")
}

func killByPidFile(paths pmdir.Paths) {
	data, err := os.ReadFile(pidFile(paths))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// WriteConfig rewrites my.ini for a version (after a port change).
// No-op when the version isn't installed.
func WriteConfig(paths pmdir.Paths, version string) error {
	if version == "" {
		return nil
	}
	if _, err := os.Stat(paths.MysqlVersionDir(version)); err != nil {
		return nil
	}
	return writeIni(paths, version)
}

// writeIni (re)writes my.ini — on every EnsureInitialized and Start, so
// a changed Port always reaches the server (idempotent).
func writeIni(paths pmdir.Paths, version string) error {
	if err := os.MkdirAll(paths.MysqlDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(paths.MysqlIni(), []byte(renderIni(paths, version, runtime.GOOS)), 0o644)
}

// renderIni is my.ini's content for a version on an OS.
func renderIni(paths pmdir.Paths, version, goos string) string {
	lines := []string{
		"# Generated by mullion — do not edit; changes are overwritten.",
		"[mysqld]",
		"basedir=" + paths.MysqlVersionDir(version),
		"datadir=" + paths.MysqlDataDir(),
		"port=" + strconv.Itoa(Port),
		"bind-address=127.0.0.1",
		// Large rows (blobs) in real projects blow past the 64MB default
		// and abort imports halfway through.
		"max_allowed_packet=512M",
		// MySQL/MariaDB both default this to 128M. A dev machine
		// restoring a multi-GB dump goes noticeably faster with more of
		// InnoDB's working set cached; 512M is a conservative bump that
		// still fits comfortably on a modest laptop.
		"innodb_buffer_pool_size=512M",
		"log-error=" + filepath.Join(paths.LogsDir(), "mysql.err"),
	}
	if !IsMaria(version) {
		// Unknown to MariaDB — it refuses to start on unknown options.
		lines = append(lines, "mysqlx=OFF")
	}
	newline := "\n"
	if goos == "windows" {
		newline = "\r\n"
	} else {
		// The default socket path, so clients connecting to `localhost`
		// (which means the socket on Unix) find the server too.
		lines = append(lines, "socket=/tmp/mysql.sock")
	}
	return strings.Join(append(lines, ""), newline)
}

// UserDatabases lists the databases on the running server, minus the
// system schemas that must not be copied between versions.
func UserDatabases(paths pmdir.Paths, version string) ([]string, error) {
	out, err := withPassword(proc.Quiet(clientExe(paths, version),
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"-N", "-e", "SHOW DATABASES")).Output()
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	system := map[string]bool{
		"mysql": true, "information_schema": true,
		"performance_schema": true, "sys": true,
	}
	var dbs []string
	for _, line := range strings.Split(string(out), "\n") {
		db := strings.TrimSpace(line)
		if db != "" && !system[db] {
			dbs = append(dbs, db)
		}
	}
	return dbs, nil
}

// DatabasesSize estimates, in bytes, the on-disk size of the given
// databases on the running server by summing information_schema's
// per-table data+index sizes. It's an approximation — mysqldump's
// textual output is a different size than the InnoDB pages it reads
// from — but it's good enough to sanity-check free disk space before
// an export (see ValidateBackupDirForExport in internal/app).
func DatabasesSize(paths pmdir.Paths, version string, dbs []string) (int64, error) {
	if len(dbs) == 0 {
		return 0, nil
	}
	quoted := make([]string, len(dbs))
	for i, db := range dbs {
		quoted[i] = "'" + strings.ReplaceAll(db, "'", "''") + "'"
	}
	stmt := fmt.Sprintf(
		"SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema IN (%s)",
		strings.Join(quoted, ","))
	out, err := withPassword(proc.Quiet(clientExe(paths, version),
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"-N", "-e", stmt)).Output()
	if err != nil {
		return 0, fmt.Errorf("estimating database size: %w", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing size estimate: %w", err)
	}
	return n, nil
}

// formatSize renders a byte count the way DumpAll/RestoreFile's
// progress line does: one decimal in MB, switching to GB once the
// number is big enough — routine now that real projects' databases run
// 10+ GB — that megabytes stop being readable at a glance.
func formatSize(bytes float64) string {
	const gb = 1e9
	if bytes >= gb {
		return fmt.Sprintf("%.2f GB", bytes/gb)
	}
	return fmt.Sprintf("%.1f MB", bytes/1e6)
}

// DumpAll writes the given databases from the running server into
// outFile using the version's own mysqldump, showing the exported size
// grow while it runs.
func DumpAll(paths pmdir.Paths, version string, dbs []string, outFile string) error {
	args := []string{
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"--single-transaction", "--routines", "--events", "--triggers",
		"--max-allowed-packet=512M",
		"--result-file=" + outFile, "--databases",
	}
	args = append(args, dbs...)
	dump := binExe(paths, version, "mysqldump", "mariadb-dump")
	cmd := withPassword(proc.Quiet(dump, args...))

	done := make(chan struct{})
	go func() {
		label := filepath.Base(outFile)
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if info, err := os.Stat(outFile); err == nil {
					fmt.Printf("\r  exporting %s  %s%s", label,
						formatSize(float64(info.Size())), term.ClearLine())
				}
			}
		}
	}()
	out, err := cmd.CombinedOutput()
	close(done)
	if err != nil {
		fmt.Println()
		return fmt.Errorf("mysqldump: %v: %s", err, out)
	}
	size := int64(0)
	if info, statErr := os.Stat(outFile); statErr == nil {
		size = info.Size()
	}
	fmt.Printf("\r  %s %s  %s%s\n", term.Green("✓"), filepath.Base(outFile),
		formatSize(float64(size)), term.ClearLine())
	return nil
}

// backupFileNames returns the per-database file names BackupDatabases
// writes for dbs, in the same order it writes them. It's a pure,
// testable stand-in for BackupDatabases' naming/no-combined-file
// behavior, since BackupDatabases itself needs a running server to
// shell out to mysqldump against.
func backupFileNames(dbs []string) []string {
	names := make([]string, len(dbs))
	for i, db := range dbs {
		names[i] = db + ".sql"
	}
	return names
}

// BackupDatabases dumps each of dbs into its own <name>.sql file under
// dir — self-contained (CREATE DATABASE + USE), so each can be
// restored alone — WITHOUT also writing a combined all-databases.sql.
// Used for setup's import of a foreign MySQL server, where a dump
// taken twice would double the disk and time an already multi-GB
// export needs. See BackupTo for the two-file version other callers
// (the panel's backup) still want.
func BackupDatabases(paths pmdir.Paths, version string, dbs []string, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := backupFileNames(dbs)
	for i, db := range dbs {
		fmt.Printf("database %d of %d: %s\n", i+1, len(dbs), db)
		if err := DumpAll(paths, version, []string{db}, filepath.Join(dir, names[i])); err != nil {
			return err
		}
	}
	return nil
}

// BackupTo dumps the given databases into dir: one <name>.sql per
// database (via BackupDatabases) plus a combined all-databases.sql for
// one-shot restores.
func BackupTo(paths pmdir.Paths, version string, dbs []string, dir string) error {
	if err := BackupDatabases(paths, version, dbs, dir); err != nil {
		return err
	}
	fmt.Println("combined file:")
	return DumpAll(paths, version, dbs, filepath.Join(dir, "all-databases.sql"))
}

var uca1400AiCi = regexp.MustCompile(`\butf8mb4_uca1400(?:_nopad)?_ai_ci\b`)
var uca1400AsCs = regexp.MustCompile(`\butf8mb4_uca1400(?:_nopad)?_as_cs\b`)
var uca1400Rest = regexp.MustCompile(`\b[a-z0-9]+_uca1400[a-z_]*\b`)

// RestoreFile replays a dump file into the running server, streaming it
// line by line (a multi-GB dump must never be loaded into memory) with
// a progress bar. Dumps taken from MariaDB use uca1400 collations MySQL
// has never heard of — when the target is MySQL each line is mapped to
// the 0900 equivalents on the fly.
func RestoreFile(paths pmdir.Paths, version, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	total := int64(0)
	if info, err := f.Stat(); err == nil {
		total = info.Size()
	}

	sanitize := !IsMaria(version)
	var fed atomic.Int64
	pr, pw := io.Pipe()
	go func() {
		// Bulk-import speed: a dump's rows are already consistent with
		// its own CREATE TABLE statements (foreign keys included), so
		// checking them again row by row during restore only costs
		// time. Both settings are understood by MySQL and MariaDB
		// alike; reset at the end in case this connection is ever
		// reused.
		if _, werr := io.WriteString(pw, "SET SESSION foreign_key_checks=0, unique_checks=0;\n"); werr != nil {
			pw.Close()
			return
		}
		br := bufio.NewReaderSize(f, 1<<20)
		for {
			line, readErr := br.ReadString('\n')
			if len(line) > 0 {
				fed.Add(int64(len(line)))
				if sanitize && strings.Contains(line, "uca1400") {
					line = uca1400AiCi.ReplaceAllString(line, "utf8mb4_0900_ai_ci")
					line = uca1400AsCs.ReplaceAllString(line, "utf8mb4_0900_as_cs")
					line = uca1400Rest.ReplaceAllString(line, "utf8mb4_general_ci")
				}
				if _, werr := io.WriteString(pw, line); werr != nil {
					return // client died; CombinedOutput reports why
				}
			}
			if readErr != nil {
				io.WriteString(pw, "SET SESSION foreign_key_checks=1, unique_checks=1;\n")
				pw.Close()
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		label := filepath.Base(file)
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				cur := fed.Load()
				if total > 0 {
					pct := cur * 100 / total
					fmt.Printf("\r  importing %s  %3d%%  %s / %s%s", label,
						pct, formatSize(float64(cur)), formatSize(float64(total)), term.ClearLine())
				} else {
					fmt.Printf("\r  importing %s  %s%s", label, formatSize(float64(cur)), term.ClearLine())
				}
			}
		}
	}()

	cmd := withPassword(proc.Quiet(clientExe(paths, version),
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"--max-allowed-packet=512M"))
	cmd.Stdin = pr
	out, err := cmd.CombinedOutput()
	close(done)
	if err != nil {
		fmt.Println()
		return fmt.Errorf("restoring dump: %v: %s", err, out)
	}
	fmt.Printf("\r  %s %s  %s imported%s\n", term.Green("✓"), filepath.Base(file),
		formatSize(float64(fed.Load())), term.ClearLine())
	return nil
}

// DataInitialized reports whether the shared data directory exists.
func DataInitialized(paths pmdir.Paths) bool {
	_, err := os.Stat(filepath.Join(paths.MysqlDataDir(), "mysql"))
	return err == nil
}

func clientExe(paths pmdir.Paths, version string) string {
	return binExe(paths, version, "mysql", "mariadb")
}

// compare orders two full versions like "8.4.6" numerically.
func compare(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		ai, _ := strconv.Atoi(as[i])
		bi, _ := strconv.Atoi(bs[i])
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// runSQL executes one statement as root on the running server.
func runSQL(paths pmdir.Paths, version, stmt string) error {
	cmd := withPassword(proc.Quiet(clientExe(paths, version),
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"-e", stmt))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mysql: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetRootPassword changes root's password on the running server (""
// removes it). The caller persists the new value in the config and
// refreshes phpMyAdmin's config.
func SetRootPassword(paths pmdir.Paths, version, newPassword string) error {
	esc := strings.ReplaceAll(newPassword, `\`, `\\`)
	esc = strings.ReplaceAll(esc, "'", `\'`)
	stmts := []string{}
	for _, host := range []string{"localhost", "127.0.0.1", "%"} {
		stmts = append(stmts, fmt.Sprintf(
			"ALTER USER IF EXISTS 'root'@'%s' IDENTIFIED BY '%s';", host, esc))
	}
	stmts = append(stmts, "FLUSH PRIVILEGES;")
	if err := runSQL(paths, version, strings.Join(stmts, " ")); err != nil {
		return err
	}
	// Verify by authenticating with the NEW password — an ALTER that
	// half-applied must not end up recorded as the working password.
	verify := proc.Quiet(clientExe(paths, version),
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"-e", "SELECT 1")
	verify.Env = append(os.Environ(), "MYSQL_PWD="+newPassword)
	if out, err := verify.CombinedOutput(); err != nil {
		return fmt.Errorf("the password change did not verify: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// validDBName keeps identifiers boring enough to quote safely.
var validDBName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Default character set and collation for new databases when none is
// asked for — what Mullion has always created.
const (
	DefaultCharset   = "utf8mb4"
	DefaultCollation = "utf8mb4_unicode_ci"
)

// charsetNameRe is the shape every character set / collation name has
// (utf8mb4, utf8mb4_0900_ai_ci, utf8mb4_uca1400_ai_ci...). Names are
// also checked against the server's own lists before they reach SQL;
// this is the belt to that pair of braces.
var charsetNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// Charset is one character set the server offers, with its collations.
type Charset struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	DefaultCollation string   `json:"defaultCollation"`
	Collations       []string `json:"collations"`
}

// CharsetInfo is what the server offers for new databases.
type CharsetInfo struct {
	Charsets        []Charset `json:"charsets"`
	ServerCharset   string    `json:"serverCharset"`
	ServerCollation string    `json:"serverCollation"`
}

// DatabaseInfo is one user database with its default character set and
// collation.
type DatabaseInfo struct {
	Name      string `json:"name"`
	Charset   string `json:"charset"`
	Collation string `json:"collation"`
}

// systemSchemas are the server's own schemas, never listed or dropped.
var systemSchemas = map[string]bool{
	"mysql": true, "information_schema": true,
	"performance_schema": true, "sys": true,
}

// querySQL runs one statement as root and returns its rows, batch mode
// (tab-separated, no headers).
func querySQL(paths pmdir.Paths, version, stmt string) (string, error) {
	out, err := withPassword(proc.Quiet(clientExe(paths, version),
		"--user=root", "--host=127.0.0.1", fmt.Sprintf("--port=%d", Port),
		"-N", "-B", "-e", stmt)).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("mysql: %v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("mysql: %w", err)
	}
	return string(out), nil
}

// tsvRows splits batch-mode output into rows of tab-separated columns.
func tsvRows(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		for i := range cols {
			cols[i] = strings.TrimSpace(cols[i])
		}
		rows = append(rows, cols)
	}
	return rows
}

const (
	// charsetsSQL: name, description, default collation.
	charsetsSQL = "SELECT CHARACTER_SET_NAME, DESCRIPTION, DEFAULT_COLLATE_NAME FROM information_schema.CHARACTER_SETS"
	// collationsSQL: charset, collation. MariaDB 10.10+ also has
	// charset-independent names (uca1400_ai_ci) with a NULL charset
	// here; their per-charset spellings come from fullCollationsSQL.
	collationsSQL = "SELECT CHARACTER_SET_NAME, COLLATION_NAME FROM information_schema.COLLATIONS WHERE CHARACTER_SET_NAME IS NOT NULL"
	// fullCollationsSQL is MariaDB 10.10+'s charset+collation pairs
	// (utf8mb4 / utf8mb4_uca1400_ai_ci); an error on older servers and
	// on MySQL, which is ignored.
	fullCollationsSQL = "SELECT CHARACTER_SET_NAME, FULL_COLLATION_NAME FROM information_schema.COLLATION_CHARACTER_SET_APPLICABILITY"
	serverCharsetSQL  = "SELECT @@character_set_server, @@collation_server"
	schemataSQL       = "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME"
)

// commonCharsets come first in the panel's list, in this order.
var commonCharsets = []string{"utf8mb4", "utf8mb3", "utf8", "latin1", "ascii", "binary", "cp1256"}

// buildCharsetInfo assembles CharsetInfo from the rows of charsetsSQL,
// collationsSQL (+ fullCollationsSQL) and serverCharsetSQL. Charsets
// are ordered common-first then by name; each charset's collations put
// its default first.
func buildCharsetInfo(charsetRows, collationRows, serverRow [][]string) CharsetInfo {
	byName := map[string]*Charset{}
	var order []string
	for _, r := range charsetRows {
		if len(r) < 3 || !charsetNameRe.MatchString(r[0]) || byName[r[0]] != nil {
			continue
		}
		byName[r[0]] = &Charset{Name: r[0], Description: r[1], DefaultCollation: r[2]}
		order = append(order, r[0])
	}
	seen := map[string]bool{}
	for _, r := range collationRows {
		if len(r) < 2 || !charsetNameRe.MatchString(r[1]) {
			continue
		}
		cs := byName[r[0]]
		if cs == nil || seen[r[0]+"\x00"+r[1]] {
			continue
		}
		seen[r[0]+"\x00"+r[1]] = true
		cs.Collations = append(cs.Collations, r[1])
	}
	info := CharsetInfo{}
	if len(serverRow) > 0 && len(serverRow[0]) >= 2 {
		info.ServerCharset, info.ServerCollation = serverRow[0][0], serverRow[0][1]
	}
	rank := map[string]int{}
	for i, n := range commonCharsets {
		rank[n] = i + 1
	}
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := rank[order[i]], rank[order[j]]
		if ri == 0 {
			ri = len(commonCharsets) + 1
		}
		if rj == 0 {
			rj = len(commonCharsets) + 1
		}
		if ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})
	for _, n := range order {
		cs := byName[n]
		// The default (and, for the server's charset, the server's
		// collation) must be choosable even if a list missed it.
		ensure := func(c string) {
			if c == "" || !charsetNameRe.MatchString(c) || seen[n+"\x00"+c] {
				return
			}
			seen[n+"\x00"+c] = true
			cs.Collations = append(cs.Collations, c)
		}
		ensure(cs.DefaultCollation)
		if n == info.ServerCharset {
			ensure(info.ServerCollation)
		}
		def := cs.DefaultCollation
		sort.SliceStable(cs.Collations, func(i, j int) bool {
			if (cs.Collations[i] == def) != (cs.Collations[j] == def) {
				return cs.Collations[i] == def
			}
			return cs.Collations[i] < cs.Collations[j]
		})
		info.Charsets = append(info.Charsets, *cs)
	}
	return info
}

// Charsets asks the running server which character sets and collations
// it offers.
func Charsets(paths pmdir.Paths, version string) (CharsetInfo, error) {
	cs, err := querySQL(paths, version, charsetsSQL)
	if err != nil {
		return CharsetInfo{}, fmt.Errorf("listing character sets: %w", err)
	}
	coll, err := querySQL(paths, version, collationsSQL)
	if err != nil {
		return CharsetInfo{}, fmt.Errorf("listing collations: %w", err)
	}
	collRows := tsvRows(coll)
	if IsMaria(version) {
		if full, err := querySQL(paths, version, fullCollationsSQL); err == nil {
			collRows = append(collRows, tsvRows(full)...)
		}
	}
	srv, _ := querySQL(paths, version, serverCharsetSQL)
	return buildCharsetInfo(tsvRows(cs), collRows, tsvRows(srv)), nil
}

// Resolve checks a requested charset/collation against what the server
// offers and fills in the missing half: a collation alone implies its
// charset; a charset alone gets that charset's default collation; both
// empty means DefaultCharset/DefaultCollation (or the server's own
// default if it lacks utf8mb4_unicode_ci).
func (info CharsetInfo) Resolve(charset, collation string) (string, string, error) {
	charset, collation = strings.TrimSpace(charset), strings.TrimSpace(collation)
	find := func(name string) *Charset {
		for i := range info.Charsets {
			if strings.EqualFold(info.Charsets[i].Name, name) {
				return &info.Charsets[i]
			}
		}
		return nil
	}
	has := func(cs *Charset, coll string) (string, bool) {
		for _, c := range cs.Collations {
			if strings.EqualFold(c, coll) {
				return c, true
			}
		}
		return "", false
	}
	if charset == "" && collation == "" {
		if cs := find(DefaultCharset); cs != nil {
			if c, ok := has(cs, DefaultCollation); ok {
				return cs.Name, c, nil
			}
			return cs.Name, cs.DefaultCollation, nil
		}
		return info.ServerCharset, info.ServerCollation, nil
	}
	if charset == "" {
		for i := range info.Charsets {
			if c, ok := has(&info.Charsets[i], collation); ok {
				return info.Charsets[i].Name, c, nil
			}
		}
		return "", "", fmt.Errorf("the server has no collation %q", collation)
	}
	cs := find(charset)
	if cs == nil {
		return "", "", fmt.Errorf("the server has no character set %q", charset)
	}
	if collation == "" {
		return cs.Name, cs.DefaultCollation, nil
	}
	c, ok := has(cs, collation)
	if !ok {
		return "", "", fmt.Errorf("collation %q does not belong to character set %s", collation, cs.Name)
	}
	return cs.Name, c, nil
}

// createDatabaseSQL renders CREATE DATABASE for an already-resolved
// charset/collation (re-checked here so nothing unvalidated is quoted).
func createDatabaseSQL(name, charset, collation string) (string, error) {
	if !validDBName.MatchString(name) {
		return "", fmt.Errorf("invalid database name %q (letters, digits, _ and - only)", name)
	}
	if !charsetNameRe.MatchString(charset) {
		return "", fmt.Errorf("invalid character set %q", charset)
	}
	stmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s CHARACTER SET %s", quoteIdent(name), charset)
	if collation != "" {
		if !charsetNameRe.MatchString(collation) {
			return "", fmt.Errorf("invalid collation %q", collation)
		}
		stmt += " COLLATE " + collation
	}
	return stmt + ";", nil
}

// CreateDatabase creates a database (utf8mb4 / utf8mb4_unicode_ci).
func CreateDatabase(paths pmdir.Paths, version, name string) error {
	return CreateDatabaseWith(paths, version, name, "", "")
}

// CreateDatabaseWith creates a database with the given character set
// and collation (either may be empty — see CharsetInfo.Resolve). Both
// are checked against the server's own lists first.
func CreateDatabaseWith(paths pmdir.Paths, version, name, charset, collation string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("invalid database name %q (letters, digits, _ and - only)", name)
	}
	if charset == "" && collation == "" {
		stmt, _ := createDatabaseSQL(name, DefaultCharset, DefaultCollation)
		return runSQL(paths, version, stmt)
	}
	info, err := Charsets(paths, version)
	if err != nil {
		return err
	}
	cs, coll, err := info.Resolve(charset, collation)
	if err != nil {
		return err
	}
	stmt, err := createDatabaseSQL(name, cs, coll)
	if err != nil {
		return err
	}
	return runSQL(paths, version, stmt)
}

// parseSchemata reads schemataSQL's rows, minus the system schemas.
func parseSchemata(out string) []DatabaseInfo {
	var dbs []DatabaseInfo
	for _, r := range tsvRows(out) {
		if systemSchemas[r[0]] {
			continue
		}
		d := DatabaseInfo{Name: r[0]}
		if len(r) >= 3 {
			d.Charset, d.Collation = r[1], r[2]
		}
		dbs = append(dbs, d)
	}
	return dbs
}

// UserDatabaseDetails lists the user databases with their default
// character set and collation (one query).
func UserDatabaseDetails(paths pmdir.Paths, version string) ([]DatabaseInfo, error) {
	out, err := querySQL(paths, version, schemataSQL)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	return parseSchemata(out), nil
}

// DropDatabase permanently deletes a database and everything in it.
func DropDatabase(paths pmdir.Paths, version, name string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("invalid database name %q", name)
	}
	system := map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}
	if system[strings.ToLower(name)] {
		return fmt.Errorf("%s is a system database — refusing to drop it", name)
	}
	return runSQL(paths, version, fmt.Sprintf("DROP DATABASE %s;", quoteIdent(name)))
}

// quoteIdent backtick-quotes an already-validated identifier.
func quoteIdent(name string) string { return "`" + name + "`" }
