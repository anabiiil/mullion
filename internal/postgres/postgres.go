// Package postgres installs and controls a local PostgreSQL server under
// ~/.mullion/postgres, following the MySQL pattern: binaries per version
// (EDB's official binary zips), one background server, a pid file, and
// a port probe to know whether it's up. Data directories are per MAJOR
// version (data-17, data-16, ...) because PostgreSQL's on-disk format
// only stays compatible within a major.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/vcredist"
)

// DefaultPort is PostgreSQL's standard port, which local dev tools expect.
const DefaultPort = 5432

// Port is the port Mullion's server listens on: DefaultPort unless the
// user moved it (config postgresPort). app.ApplyPortConfig sets it from
// the config at startup; mullion.conf, the client tools and the
// readiness probe all read it at call time.
var Port = DefaultPort

// Superuser is the cluster's superuser role.
const Superuser = "postgres"

// Password is the superuser's password ("" until the user sets one;
// the server then trusts connections from 127.0.0.1/::1 only). The app
// loads it from the config; every client invocation here honors it.
var Password string

// Label renders a version for humans: "PostgreSQL 17.6".
func Label(version string) string { return "PostgreSQL " + version }

func dir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "postgres") }

// BaseDir is where every installed version's binaries and every
// major's data directory (data-<major>) live — used by `mullion
// postgres uninstall` to remove the binaries while optionally sparing
// the data-* subdirectories.
func BaseDir(paths pmdir.Paths) string { return dir(paths) }

func versionDir(paths pmdir.Paths, version string) string {
	return filepath.Join(dir(paths), version)
}

// BinDir holds the server and client tools (psql, pg_dump, ...).
func BinDir(paths pmdir.Paths, version string) string {
	return filepath.Join(versionDir(paths, version), "bin")
}

func exe(paths pmdir.Paths, version, name string) string {
	return filepath.Join(BinDir(paths, version), pmdir.ExeName(name))
}

// DataDir is the cluster directory, shared by every version of a major.
func DataDir(paths pmdir.Paths, version string) string {
	return filepath.Join(dir(paths), "data-"+Major(version))
}

// DataInitialized reports whether the major's cluster exists.
func DataInitialized(paths pmdir.Paths, version string) bool {
	_, err := os.Stat(filepath.Join(DataDir(paths, version), "PG_VERSION"))
	return err == nil
}

// Installed lists the installed versions, oldest first.
func Installed(paths pmdir.Paths) []string {
	entries, err := os.ReadDir(dir(paths))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !versionRe.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(exe(paths, e.Name(), "postgres")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return compare(out[i], out[j]) < 0 })
	return out
}

// Install downloads and unpacks a release into the versions directory.
// It is a no-op if the version is already installed.
func Install(ctx context.Context, paths pmdir.Paths, version string) error {
	if _, err := os.Stat(exe(paths, version, "postgres")); err == nil {
		return nil
	}
	if err := platformSupported(); err != nil {
		return err
	}
	if !versionRe.MatchString(version) {
		return fmt.Errorf("invalid PostgreSQL version %q", version)
	}
	// postgres.exe needs the Visual C++ runtime, which EDB's zip lacks.
	if err := vcredist.Ensure(ctx, paths); err != nil {
		return err
	}

	fmt.Printf("Downloading %s...\n", Label(version))
	staging := filepath.Join(paths.TmpDir(), "postgres-extract")
	defer os.RemoveAll(staging)
	var lastErr error
	for _, url := range downloadURLs(version) {
		os.RemoveAll(staging)
		if lastErr = fetchZip(ctx, url, staging, paths.TmpDir(), wantEntry); lastErr != nil {
			// Only an HTTP miss means "try the next build number"; a
			// local failure would fail them all.
			if strings.Contains(lastErr.Error(), "HTTP 4") {
				continue
			}
			return fmt.Errorf("downloading %s: %w", Label(version), lastErr)
		}
		inner, err := findServerDir(staging)
		if err != nil {
			return err
		}
		prepareBinaries(inner)
		if err := checkRuns(filepath.Join(inner, "bin", pmdir.ExeName("postgres"))); err != nil {
			return err
		}
		if err := os.MkdirAll(dir(paths), 0o755); err != nil {
			return err
		}
		os.RemoveAll(versionDir(paths, version)) // a half-finished earlier attempt
		return os.Rename(inner, versionDir(paths, version))
	}
	if lastErr == nil {
		lastErr = errors.New("no download source for this platform")
	}
	return fmt.Errorf("downloading %s: %w", Label(version), lastErr)
}

// findServerDir locates the extracted directory holding bin/postgres
// (EDB's zips wrap everything in pgsql/).
func findServerDir(staging string) (string, error) {
	candidates := []string{staging}
	if entries, err := os.ReadDir(staging); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(staging, e.Name()))
			}
		}
	}
	for _, d := range candidates {
		if _, err := os.Stat(filepath.Join(d, "bin", pmdir.ExeName("postgres"))); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("unexpected archive layout: no bin/%s under %s", pmdir.ExeName("postgres"), staging)
}

// checkRuns executes `postgres --version` so a broken download (or, on
// Windows, a missing Visual C++ runtime) fails the install instead of
// the first start.
func checkRuns(exePath string) error {
	cmd := proc.Quiet(exePath, "--version")
	timer := time.AfterFunc(60*time.Second, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	out, err := cmd.CombinedOutput()
	timer.Stop()
	if err != nil {
		return fmt.Errorf("%s does not run: %v: %s%s", filepath.Base(exePath), err,
			strings.TrimSpace(string(out)), runHint)
	}
	return nil
}

// EnsureInitialized creates the major's cluster on first use (superuser
// "postgres", UTF-8, C locale) and (re)writes Mullion's configuration:
// mullion.conf (included from postgresql.conf) and pg_hba.conf, whose
// auth method follows Password.
func EnsureInitialized(paths pmdir.Paths, version string) error {
	data := DataDir(paths, version)
	if !DataInitialized(paths, version) {
		// initdb refuses a non-empty directory; a leftover from a failed
		// attempt holds nothing worth keeping (no PG_VERSION).
		os.RemoveAll(data)
		if err := os.MkdirAll(dir(paths), 0o755); err != nil {
			return err
		}
		fmt.Printf("Initializing the %s data directory...\n", Label(version))
		args := []string{"-D", data, "-U", Superuser, "-E", "UTF8", "--locale=C"}
		if Password != "" {
			pwFile := filepath.Join(paths.TmpDir(), "postgres-pw")
			if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(pwFile, []byte(Password+"\n"), 0o600); err != nil {
				return err
			}
			defer os.Remove(pwFile)
			args = append(args, "--pwfile="+pwFile, "--auth=scram-sha-256")
		} else {
			args = append(args, "--auth=trust")
		}
		cmd := proc.Quiet(exe(paths, version, "initdb"), args...)
		cmd.Env = cleanEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			os.RemoveAll(data)
			return fmt.Errorf("initdb: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if err := writeConf(data); err != nil {
		return err
	}
	return writeHba(data, Password != "")
}

const confName = "mullion.conf"

// renderConf is Mullion's server configuration. Logging goes to the
// file pg_ctl -l opens in the logs directory.
func renderConf(goos string) string {
	lines := []string{
		"# Generated by mullion — do not edit; changes are overwritten.",
		"listen_addresses = '127.0.0.1'",
		"port = " + strconv.Itoa(Port),
		"max_connections = 100",
		"password_encryption = 'scram-sha-256'",
		"log_line_prefix = '%m [%p] %q%u@%d '",
	}
	if goos != "windows" {
		// The default socket path, so `psql` without -h finds the server.
		lines = append(lines, "unix_socket_directories = '/tmp'")
	}
	return strings.Join(lines, "\n") + "\n"
}

// renderHba is the whole pg_hba.conf: local connections only, trusted
// until a password is set, SCRAM after.
func renderHba(goos string, password bool) string {
	method := "trust"
	if password {
		method = "scram-sha-256"
	}
	lines := []string{
		"# Generated by mullion — do not edit; changes are overwritten.",
		"# TYPE  DATABASE     USER  ADDRESS       METHOD",
	}
	if goos != "windows" {
		lines = append(lines, "local   all          all                 "+method)
	}
	lines = append(lines,
		"host    all          all   127.0.0.1/32  "+method,
		"host    all          all   ::1/128       "+method,
		"host    replication  all   127.0.0.1/32  "+method,
		"host    replication  all   ::1/128       "+method,
	)
	return strings.Join(lines, "\n") + "\n"
}

// WriteConfig rewrites the major's mullion.conf (after a port change).
// No-op until the cluster exists — EnsureInitialized writes it then.
func WriteConfig(paths pmdir.Paths, version string) error {
	if version == "" || !DataInitialized(paths, version) {
		return nil
	}
	return writeConf(DataDir(paths, version))
}

// writeConf writes mullion.conf and makes sure postgresql.conf includes
// it (last, so its settings win).
func writeConf(data string) error {
	if err := os.WriteFile(filepath.Join(data, confName), []byte(renderConf(runtime.GOOS)), 0o600); err != nil {
		return err
	}
	main := filepath.Join(data, "postgresql.conf")
	body, err := os.ReadFile(main)
	if err != nil {
		return err
	}
	include := "include_if_exists = '" + confName + "'"
	if strings.Contains(string(body), include) {
		return nil
	}
	s := string(body)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	s += "\n# Mullion's settings (" + confName + ") override the defaults above.\n" + include + "\n"
	return os.WriteFile(main, []byte(s), 0o600)
}

func writeHba(data string, password bool) error {
	return os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte(renderHba(runtime.GOOS, password)), 0o600)
}

// cleanEnv is the environment for the server and client tools, minus
// any PG* variables from the user's shell that would redirect them
// (PGHOST, PGDATA, PGPORT, PGSERVICE, ...).
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PG") {
			env = append(env, kv)
		}
	}
	return env
}

func logFile(paths pmdir.Paths) string { return filepath.Join(paths.LogsDir(), "postgres.log") }

func pidFile(paths pmdir.Paths) string { return filepath.Join(paths.PidsDir(), "postgres.pid") }

// pgCtl runs pg_ctl with its output going to a file, never a pipe: on
// Windows the server pg_ctl spawns inherits pg_ctl's handles, and a
// pipe held open by the server would make us wait forever.
func pgCtl(paths pmdir.Paths, version string, args ...string) (string, error) {
	if err := os.MkdirAll(paths.LogsDir(), 0o755); err != nil {
		return "", err
	}
	outPath := filepath.Join(paths.LogsDir(), "postgres-ctl.log")
	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	cmd := proc.Quiet(exe(paths, version, "pg_ctl"), args...)
	cmd.Env = cleanEnv()
	cmd.Stdout = out
	cmd.Stderr = out
	runErr := cmd.Run()
	out.Close()
	text, _ := os.ReadFile(outPath)
	return strings.TrimSpace(string(text)), runErr
}

// Start launches the server through pg_ctl (which also drops
// Administrator rights on Windows — postgres refuses to run with them)
// and waits for the port to come up.
func Start(paths pmdir.Paths, version string) error {
	if Running() {
		return nil
	}
	if err := EnsureInitialized(paths, version); err != nil {
		return err
	}
	data := DataDir(paths, version)
	// A crash leaves postmaster.pid behind; on Windows its pid may since
	// belong to an unrelated process, which blocks the start.
	if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err == nil {
		if _, err := pgCtl(paths, version, "status", "-D", data); exitCode(err) == 3 {
			os.Remove(filepath.Join(data, "postmaster.pid"))
		}
	}
	out, err := pgCtl(paths, version, "start", "-D", data, "-l", logFile(paths), "-w", "-t", "60")
	if err != nil {
		return fmt.Errorf("starting %s: %v: %s%s", Label(version), err, out, logTail(paths))
	}
	if pid := postmasterPid(data); pid > 0 {
		_ = os.MkdirAll(paths.PidsDir(), 0o755)
		_ = os.WriteFile(pidFile(paths), []byte(strconv.Itoa(pid)), 0o644)
	}
	for i := 0; i < 40; i++ {
		if Running() {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("postgres did not start listening on port %d (see %s)", Port, logFile(paths))
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// logTail returns the last lines of the server log for error messages.
func logTail(paths pmdir.Paths) string {
	body, err := os.ReadFile(logFile(paths))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(body), "\r\n"), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return "\n" + strings.Join(lines, "\n")
}

// postmasterPid reads the server's pid from its lock file.
func postmasterPid(data string) int {
	body, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(string(body), "\n")
	pid, _ := strconv.Atoi(strings.TrimSpace(first))
	return pid
}

// Stop shuts the server down (fast mode: open sessions are
// disconnected, everything is flushed), falling back to killing the
// recorded pid, and waits until the port is free.
func Stop(paths pmdir.Paths, version string) error {
	defer os.Remove(pidFile(paths))
	data := DataDir(paths, version)
	if _, err := os.Stat(pidFile(paths)); err != nil && postmasterPid(data) == 0 {
		return nil // ours isn't running (the port may belong to another server)
	}
	out, err := pgCtl(paths, version, "stop", "-D", data, "-m", "fast", "-w", "-t", "60")
	if err != nil && Running() {
		killPid(paths, data)
		err = fmt.Errorf("pg_ctl stop: %v: %s (killed the process instead)", err, out)
	} else {
		err = nil // stopped, or it wasn't running after all (stale pid file)
	}
	for i := 0; i < 120 && Running(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func killPid(paths pmdir.Paths, data string) {
	pid := postmasterPid(data)
	if pid <= 0 {
		body, err := os.ReadFile(pidFile(paths))
		if err != nil {
			return
		}
		pid, _ = strconv.Atoi(strings.TrimSpace(string(body)))
	}
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// Running reports whether something is serving the PostgreSQL port.
func Running() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", Port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// psql runs SQL (fed on stdin, so secrets stay out of `ps`) as the
// superuser against the running server and returns its unaligned,
// tuples-only output.
func psql(paths pmdir.Paths, version, password, sql string) (string, error) {
	cmd := proc.Quiet(exe(paths, version, "psql"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(Port), "-U", Superuser, "-d", "postgres",
		"-X", "-w", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1")
	cmd.Env = append(cleanEnv(), "PGCONNECT_TIMEOUT=10")
	if password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+password)
	}
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// systemDBs are never listed, created over or dropped.
var systemDBs = map[string]bool{"postgres": true, "template0": true, "template1": true}

// UserDatabases lists the databases on the running server, minus the
// maintenance database and the templates.
func UserDatabases(paths pmdir.Paths, version string) ([]string, error) {
	out, err := psql(paths, version, Password,
		"SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY datname;")
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	var dbs []string
	for _, line := range strings.Split(out, "\n") {
		if db := strings.TrimSpace(line); db != "" && !systemDBs[db] {
			dbs = append(dbs, db)
		}
	}
	return dbs, nil
}

// validDBName keeps identifiers boring enough to quote safely.
var validDBName = regexp.MustCompile(`^[A-Za-z0-9_]{1,63}$`)

func checkName(name string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("invalid database name %q (letters, digits and _ only, at most 63)", name)
	}
	return nil
}

// quoteIdent double-quotes an already-validated identifier (so its
// case is kept: "MyApp" stays MyApp, as clients will ask for it).
func quoteIdent(name string) string { return `"` + name + `"` }

// quoteLiteral renders a SQL string literal (standard_conforming_strings
// is on, so only quotes need doubling).
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// CreateDatabase creates a database (UTF-8, like the cluster); an
// existing one is left alone.
func CreateDatabase(paths pmdir.Paths, version, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if systemDBs[strings.ToLower(name)] {
		return fmt.Errorf("%s is a system database", name)
	}
	out, err := psql(paths, version, Password,
		"SELECT 1 FROM pg_database WHERE datname = "+quoteLiteral(name)+";")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "1" {
		return nil
	}
	_, err = psql(paths, version, Password,
		"CREATE DATABASE "+quoteIdent(name)+" ENCODING 'UTF8';")
	return err
}

// dropSQL renders the statements that drop a database, disconnecting
// its sessions first (WITH (FORCE) since 13; by hand before).
func dropSQL(version, name string) string {
	if major, _ := strconv.Atoi(Major(version)); major >= 13 {
		return "DROP DATABASE " + quoteIdent(name) + " WITH (FORCE);"
	}
	return "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = " +
		quoteLiteral(name) + " AND pid <> pg_backend_pid();\n" +
		"DROP DATABASE " + quoteIdent(name) + ";"
}

// DropDatabase permanently deletes a database and everything in it.
func DropDatabase(paths pmdir.Paths, version, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if systemDBs[strings.ToLower(name)] {
		return fmt.Errorf("%s is a system database — refusing to drop it", name)
	}
	_, err := psql(paths, version, Password, dropSQL(version, name))
	return err
}

// SetPassword changes the superuser's password on the running server
// ("" removes it and goes back to trusting local connections),
// switches pg_hba.conf to match, reloads, and verifies a login with the
// new setting. On success it updates Password; the caller persists the
// value in the config.
func SetPassword(paths pmdir.Paths, version, password string) error {
	data := DataDir(paths, version)
	if password != "" {
		// Set it while the current auth still works, then require it.
		if _, err := psql(paths, version, Password,
			"ALTER ROLE "+Superuser+" WITH PASSWORD "+quoteLiteral(password)+";"); err != nil {
			return err
		}
		if err := writeHba(data, true); err != nil {
			return err
		}
	} else if err := writeHba(data, false); err != nil {
		return err
	}
	if out, err := pgCtl(paths, version, "reload", "-D", data); err != nil {
		return fmt.Errorf("pg_ctl reload: %v: %s", err, out)
	}
	// The reload is asynchronous: retry the login briefly.
	var err error
	for i := 0; i < 20; i++ {
		if _, err = psql(paths, version, password, "SELECT 1;"); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("the password change did not verify: %w", err)
	}
	if password == "" {
		// Trusted now; drop the stale secret so it can't linger.
		if _, err := psql(paths, version, "", "ALTER ROLE "+Superuser+" WITH PASSWORD NULL;"); err != nil {
			return err
		}
	}
	Password = password
	return nil
}
