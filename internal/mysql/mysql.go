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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"pm/internal/archive"
	"pm/internal/download"
	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/term"
)

// Port is fixed: local dev tools (phpMyAdmin included) expect the default.
const Port = 3306

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
			continue
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
	return nil
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

func writeIni(paths pmdir.Paths, version string) error {
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
		"log-error=" + filepath.Join(paths.LogsDir(), "mysql.err"),
	}
	if !IsMaria(version) {
		// Unknown to MariaDB — it refuses to start on unknown options.
		lines = append(lines, "mysqlx=OFF")
	}
	newline := "\n"
	if runtime.GOOS == "windows" {
		newline = "\r\n"
	} else {
		// The default socket path, so clients connecting to `localhost`
		// (which means the socket on Unix) find the server too.
		lines = append(lines, "socket=/tmp/mysql.sock")
	}
	content := strings.Join(append(lines, ""), newline)
	if err := os.MkdirAll(paths.MysqlDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(paths.MysqlIni(), []byte(content), 0o644)
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
					fmt.Printf("\r  exporting %s  %.1f MB%s", label,
						float64(info.Size())/1e6, term.ClearLine())
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
	fmt.Printf("\r  %s %s  %.1f MB%s\n", term.Green("✓"), filepath.Base(outFile),
		float64(size)/1e6, term.ClearLine())
	return nil
}

// BackupTo dumps the given databases into dir: one <name>.sql per
// database (each self-contained with its CREATE DATABASE, so it can be
// imported alone) plus a combined all-databases.sql for one-shot
// restores.
func BackupTo(paths pmdir.Paths, version string, dbs []string, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, db := range dbs {
		fmt.Printf("database %d of %d: %s\n", i+1, len(dbs), db)
		if err := DumpAll(paths, version, []string{db}, filepath.Join(dir, db+".sql")); err != nil {
			return err
		}
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
					fmt.Printf("\r  importing %s  %3d%%  %.1f / %.1f MB%s", label,
						pct, float64(cur)/1e6, float64(total)/1e6, term.ClearLine())
				} else {
					fmt.Printf("\r  importing %s  %.1f MB%s", label, float64(cur)/1e6, term.ClearLine())
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
	fmt.Printf("\r  %s %s  %.1f MB imported%s\n", term.Green("✓"), filepath.Base(file),
		float64(fed.Load())/1e6, term.ClearLine())
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

// CreateDatabase creates a database (utf8mb4).
func CreateDatabase(paths pmdir.Paths, version, name string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("invalid database name %q (letters, digits, _ and - only)", name)
	}
	return runSQL(paths, version,
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;", quoteIdent(name)))
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
