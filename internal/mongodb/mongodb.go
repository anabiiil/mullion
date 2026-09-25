// Package mongodb installs and controls a local MongoDB server under
// ~/.mullion/mongodb, following the same pattern as MySQL: binaries per
// version, one detached background process, a pid file, and a port
// probe to know whether it's up. Database operations go through the
// official shell (mongosh) — no driver is linked into Mullion.
//
// Layout (under paths.Home/mongodb):
//
//	<version>/bin/mongod        the server, one directory per version
//	mongosh-<v>/bin/mongosh     the shell, shared by every server version
//	data/                       the shared dbPath
//	mongod.conf                 generated config (overwritten on start)
package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
)

// DefaultPort is MongoDB's standard port, which local dev tools and
// connection strings expect.
const DefaultPort = 27017

// Port is the port Mullion's server listens on: DefaultPort unless the
// user moved it (config mongoPort). app.ApplyPortConfig sets it from the
// config at startup; mongod.conf, the shell/tool URIs and the readiness
// probe all read it at call time.
var Port = DefaultPort

// DefaultSeries is what a bare install gets: the newest major release
// line, the one drivers and ORMs test against. Rapid releases (8.2,
// 8.3, ...) stay available via "latest" or an explicit series.
const DefaultSeries = "8.0"

// Label renders a version for humans: "MongoDB 8.0.12".
func Label(version string) string { return "MongoDB " + version }

// ConnectionURI is what apps put in their .env (no auth: local dev only,
// and the server only listens on 127.0.0.1).
func ConnectionURI() string { return fmt.Sprintf("mongodb://127.0.0.1:%d", Port) }

func baseDir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "mongodb") }

// BaseDir is where every installed server version, mongosh, the
// Database Tools, and the shared data directory live — used by
// `mullion mongo uninstall` to remove the binaries while optionally
// sparing the data directory.
func BaseDir(paths pmdir.Paths) string { return baseDir(paths) }

func versionDir(paths pmdir.Paths, version string) string {
	return filepath.Join(baseDir(paths), version)
}

func serverExe(paths pmdir.Paths, version string) string {
	return filepath.Join(versionDir(paths, version), "bin", pmdir.ExeName("mongod"))
}

func confFile(paths pmdir.Paths) string { return filepath.Join(baseDir(paths), "mongod.conf") }

func logFile(paths pmdir.Paths) string { return filepath.Join(paths.LogsDir(), "mongodb.log") }

func pidFile(paths pmdir.Paths) string { return filepath.Join(paths.PidsDir(), "mongodb.pid") }

// DataDir is the shared dbPath every installed version uses.
func DataDir(paths pmdir.Paths) string { return filepath.Join(baseDir(paths), "data") }

// DataInitialized reports whether a server has ever run on the data
// directory (WiredTiger writes its metadata file on first start).
func DataInitialized(paths pmdir.Paths) bool {
	_, err := os.Stat(filepath.Join(DataDir(paths), "WiredTiger"))
	return err == nil
}

var fullVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Installed lists the server versions present on disk, newest first.
func Installed(paths pmdir.Paths) []string {
	entries, err := os.ReadDir(baseDir(paths))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !fullVersionRe.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(serverExe(paths, e.Name())); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return compare(out[i], out[j]) > 0 })
	return out
}

// MongoshPath returns the newest installed mongosh, or "" when none is.
func MongoshPath(paths pmdir.Paths) string {
	entries, err := os.ReadDir(baseDir(paths))
	if err != nil {
		return ""
	}
	best, bestPath := "", ""
	for _, e := range entries {
		v, ok := strings.CutPrefix(e.Name(), "mongosh-")
		if !e.IsDir() || !ok || !fullVersionRe.MatchString(v) {
			continue
		}
		p := filepath.Join(baseDir(paths), e.Name(), "bin", pmdir.ExeName("mongosh"))
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if best == "" || compare(v, best) > 0 {
			best, bestPath = v, p
		}
	}
	return bestPath
}

// EnsureInitialized creates the data directory and (re)writes
// mongod.conf. Unlike MySQL there is no separate init step: mongod
// creates its files on first start.
func EnsureInitialized(paths pmdir.Paths, version string) error {
	for _, dir := range []string{baseDir(paths), DataDir(paths), paths.LogsDir(), paths.PidsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(confFile(paths), []byte(renderConf(paths)), 0o644)
}

// WriteConfig rewrites mongod.conf (after a port change). No-op when
// MongoDB was never set up — EnsureInitialized writes it then.
func WriteConfig(paths pmdir.Paths) error {
	if _, err := os.Stat(baseDir(paths)); err != nil {
		return nil
	}
	return os.WriteFile(confFile(paths), []byte(renderConf(paths)), 0o644)
}

// yamlQuote single-quotes a YAML scalar (a doubled quote is the only escape), so
// Windows paths keep their backslashes verbatim.
func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// renderConf builds mongod.conf: loopback only, no auth (local dev), a
// small WiredTiger cache (the default grabs half the machine's RAM),
// and no diagnostic-data files churning the disk.
func renderConf(paths pmdir.Paths) string {
	lines := []string{
		"# Generated by mullion — do not edit; changes are overwritten.",
		"net:",
		"  bindIp: 127.0.0.1",
		"  port: " + strconv.Itoa(Port),
		"storage:",
		"  dbPath: " + yamlQuote(DataDir(paths)),
		"  wiredTiger:",
		"    engineConfig:",
		"      cacheSizeGB: 0.25",
		"systemLog:",
		"  destination: file",
		"  path: " + yamlQuote(logFile(paths)),
		"  logAppend: true",
		"setParameter:",
		"  diagnosticDataCollectionEnabled: false",
	}
	if runtime.GOOS != "windows" {
		// No /tmp/mongodb-<port>.sock: clients use TCP, and a stale
		// socket left by a crash would block the next start. (Windows
		// builds don't know the option and would refuse the config.)
		lines = slices.Insert(lines, 4, "  unixDomainSocket:", "    enabled: false")
	}
	return strings.Join(lines, "\n") + "\n"
}

// Start launches mongod detached and waits for the port to come up.
// mongod's own fork mode is Unix-only, so the process is detached the
// same way on every OS (proc.Detach).
func Start(paths pmdir.Paths, version string) error {
	if Running() {
		return nil
	}
	exe := serverExe(paths, version)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("%s is not installed", Label(version))
	}
	if err := EnsureInitialized(paths, version); err != nil {
		return err
	}
	// mongod logs to its own file (systemLog); this only catches what it
	// prints before the config is parsed.
	out, err := os.OpenFile(filepath.Join(paths.LogsDir(), "mongodb-console.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := proc.Quiet(exe, "--config", confFile(paths))
	cmd.Dir = versionDir(paths, version)
	cmd.Stdout = out
	cmd.Stderr = out
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting mongod: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidFile(paths), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	// Reap the child if it dies during startup so a bad config or a
	// locked data dir is reported right away instead of after a timeout.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	for i := 0; i < 240; i++ {
		if Running() {
			return nil
		}
		select {
		case err := <-exited:
			os.Remove(pidFile(paths))
			return fmt.Errorf("mongod exited during startup (%v)%s — see %s", err, lastLogError(paths), logFile(paths))
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("mongod did not start listening on port %d (see %s)", Port, logFile(paths))
}

// lastLogError digs the first error of the latest run out of mongod's
// structured (JSON-lines) log — the root cause, not the shutdown noise
// that follows it.
func lastLogError(paths pmdir.Paths) string {
	data, err := os.ReadFile(logFile(paths))
	if err != nil {
		return ""
	}
	if len(data) > 256<<10 {
		data = data[len(data)-256<<10:]
	}
	type entry struct {
		S    string         `json:"s"`
		Msg  string         `json:"msg"`
		Attr map[string]any `json:"attr"`
	}
	var run []entry
	for _, line := range strings.Split(string(data), "\n") {
		var e entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.Msg == "Build Info" { // logged first by every start
			run = run[:0]
		}
		run = append(run, e)
	}
	for _, e := range run {
		if e.S == "E" || e.S == "F" {
			msg := e.Msg
			if m, ok := e.Attr["message"].(map[string]any); ok && m["msg"] != nil {
				msg = fmt.Sprint(m["msg"]) // WiredTiger nests its text
			} else if v, ok := e.Attr["error"]; ok {
				msg += fmt.Sprintf(": %v", v)
			}
			return ": " + msg
		}
	}
	return ""
}

// Stop shuts the server down cleanly — through mongosh's shutdown
// command, or SIGTERM (mongod's clean-shutdown signal) when no shell is
// installed — escalating to a hard kill of the recorded pid, and waits
// until the port closes and the data files are released. (`mongod
// --shutdown` would be simpler but exists only on Linux.)
func Stop(paths pmdir.Paths, version string) error {
	defer os.Remove(pidFile(paths))
	if !Running() {
		// Nothing to stop. A leftover pid file is not trusted with a
		// kill: the pid may belong to an unrelated process by now.
		return nil
	}
	stopped := false
	if sh := MongoshPath(paths); sh != "" {
		// The server drops the connection as it goes down, which the
		// shell may report as an error — success is judged by the port.
		_ = proc.Quiet(sh, shellArgs("try { db.getSiblingDB('admin').adminCommand({shutdown: 1}) } catch (e) {}")...).Run()
		stopped = waitPortClosed(60 * time.Second)
	}
	if !stopped && runtime.GOOS != "windows" {
		signalPid(paths, false)
		stopped = waitPortClosed(60 * time.Second)
	}
	if !stopped {
		signalPid(paths, true)
		if !waitPortClosed(10 * time.Second) {
			return fmt.Errorf("mongod is still listening on port %d", Port)
		}
	}
	// The port closes before mongod has checkpointed and released its
	// files; a clean shutdown ends with mongod.lock emptied.
	lock := filepath.Join(DataDir(paths), "mongod.lock")
	for i := 0; i < 120; i++ {
		if info, err := os.Stat(lock); err != nil || info.Size() == 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func waitPortClosed(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !Running() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return !Running()
}

// signalPid asks the recorded mongod to terminate (SIGTERM is mongod's
// clean-shutdown signal); hard kills it when force is set or the
// platform has no SIGTERM (Windows).
func signalPid(paths pmdir.Paths, force bool) {
	pid := recordedPid(paths)
	if pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if !force && runtime.GOOS != "windows" {
		if p.Signal(syscall.SIGTERM) == nil {
			return
		}
	}
	_ = p.Kill()
}

// recordedPid reads our pid file, falling back to the pid mongod itself
// writes into mongod.lock.
func recordedPid(paths pmdir.Paths) int {
	for _, f := range []string{pidFile(paths), filepath.Join(DataDir(paths), "mongod.lock")} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// Running reports whether something is serving the MongoDB port.
func Running() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", Port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// shellArgs builds a non-interactive mongosh invocation against the
// local server.
func shellArgs(js string) []string {
	return []string{"--quiet", "--norc", "--host", "127.0.0.1", "--port", strconv.Itoa(Port), "--eval", js}
}

// runJS evaluates a script with mongosh and returns its stdout.
func runJS(paths pmdir.Paths, js string) (string, error) {
	sh := MongoshPath(paths)
	if sh == "" {
		return "", fmt.Errorf("mongosh is not installed (install MongoDB first)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := proc.Quiet(sh, shellArgs(js)...)
	cmd = withContext(ctx, cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("mongosh: %v: %s", err, msg)
	}
	return stdout.String(), nil
}

// withContext rebuilds a prepared command under a context, keeping the
// platform attributes proc.Quiet set (no console window on Windows).
func withContext(ctx context.Context, cmd *exec.Cmd) *exec.Cmd {
	c := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	c.SysProcAttr = cmd.SysProcAttr
	c.Env = cmd.Env
	return c
}

// systemDBs are MongoDB's internal databases.
var systemDBs = map[string]bool{"admin": true, "local": true, "config": true}

// UserDatabases lists the databases on the running server, minus the
// system ones.
func UserDatabases(paths pmdir.Paths) ([]string, error) {
	out, err := runJS(paths,
		`print(JSON.stringify(db.getSiblingDB('admin').adminCommand({listDatabases: 1, nameOnly: true}).databases.map(d => d.name)))`)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	names, err := parseNameList(out)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	var dbs []string
	for _, n := range names {
		if !systemDBs[strings.ToLower(n)] {
			dbs = append(dbs, n)
		}
	}
	sort.Strings(dbs)
	return dbs, nil
}

// parseNameList finds the JSON array line in mongosh's output (skipping
// any warnings the shell printed around it).
func parseNameList(out string) ([]string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "[") {
			continue
		}
		var names []string
		if err := json.Unmarshal([]byte(line), &names); err == nil {
			return names, nil
		}
	}
	return nil, fmt.Errorf("unexpected mongosh output: %q", strings.TrimSpace(out))
}

// validDBName keeps names boring enough to embed in a script safely and
// valid as MongoDB database names (and as directory names on Windows).
var validDBName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

func checkName(name string) error {
	if !validDBName.MatchString(name) {
		return fmt.Errorf("invalid database name %q (letters, digits, _ and - only, up to 63)", name)
	}
	return nil
}

// jsString renders a validated name as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// placeholderCollection is created in new databases: MongoDB creates
// databases lazily and doesn't list (or keep) one until it holds a
// collection, so "create database" means "create a collection in it".
// It's harmless to delete once the app has created its own.
const placeholderCollection = "_mullion"

// CreateDatabase makes a database show up (and persist) by creating a
// placeholder collection in it; a database that already has
// collections is left as it is.
func CreateDatabase(paths pmdir.Paths, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if systemDBs[strings.ToLower(name)] {
		return fmt.Errorf("%s is a system database", name)
	}
	js := fmt.Sprintf(`const d = db.getSiblingDB(%s); if (d.getCollectionNames().length === 0) { d.createCollection(%s) }; print("ok")`,
		jsString(name), jsString(placeholderCollection))
	_, err := runJS(paths, js)
	return err
}

// DropDatabase permanently deletes a database and everything in it.
func DropDatabase(paths pmdir.Paths, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if systemDBs[strings.ToLower(name)] {
		return fmt.Errorf("%s is a system database — refusing to drop it", name)
	}
	js := fmt.Sprintf(`const r = db.getSiblingDB(%s).dropDatabase(); if (!r.ok) { throw new Error(JSON.stringify(r)) }; print("ok")`,
		jsString(name))
	_, err := runJS(paths, js)
	return err
}

// compare orders two full versions like "8.0.12" numerically.
func compare(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}
