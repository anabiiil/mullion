package frankenphp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
)

// Run is one site's FrankenPHP server as Ensure starts it.
type Run struct {
	Paths      pmdir.Paths
	Bin        string // the frankenphp executable
	Site       SiteConf
	ProjectDir string // the project root (worker mode runs artisan there)
	// Worker runs Laravel Octane's own `octane:frankenphp` instead of
	// `frankenphp run` with our Caddyfile.
	Worker bool
	// PHPCLI is the argv prefix that runs artisan in worker mode:
	// [<bin>, "php-cli"].
	PHPCLI []string
}

func pidFile(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.PidsDir(), "franken-"+site+".pid")
}
func hashFile(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.PidsDir(), "franken-"+site+".hash")
}
func logFilePath(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.LogsDir(), "franken-"+site+".log")
}
func caddyfilePath(paths pmdir.Paths, site string) string {
	return filepath.Join(paths.FrankenDir(), "sites", site, "Caddyfile")
}

// readyTimeout is how long a fresh server gets to open its port.
const readyTimeout = 30 * time.Second

// stopGrace is how long Stop waits after the polite kill before forcing
// it. A variable so tests can shorten it.
var stopGrace = 10 * time.Second

// Running reports whether the site's FrankenPHP server process is alive.
func Running(paths pmdir.Paths, site string) bool {
	pid := readInt(pidFile(paths, site))
	return pid > 0 && processAlive(pid)
}

// Ensure starts the site's FrankenPHP server, or restarts it when its
// configuration (Caddyfile, mode or binary) changed, and returns the port
// Caddy should proxy to.
func Ensure(r Run) (int, error) {
	port, err := ensure(r)
	if err != nil {
		// Callers only print this to the agent's stdout, and the UI shows
		// the log tail — so the reason itself must land in the log.
		reason := strings.SplitN(err.Error(), "\n", 2)[0]
		reason = strings.TrimSuffix(reason, " — last log lines:")
		if f, ferr := os.OpenFile(logFilePath(r.Paths, r.Site.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			fmt.Fprintf(f, "---- %s: mullion: %s ----\n", time.Now().Format(time.RFC3339), reason)
			f.Close()
		}
	}
	return port, err
}

func ensure(r Run) (int, error) {
	paths, site, port := r.Paths, r.Site.Name, r.Site.Port
	if site == "" || port <= 0 {
		return 0, fmt.Errorf("FrankenPHP server needs a site name and a port (got %q, %d)", site, port)
	}
	conf := Caddyfile(r.Site)
	hash := configHash(conf, r.Worker, r.Bin)
	// The port check guards against a recycled pid (after a reboot) that
	// merely looks alive.
	if Running(paths, site) && readString(hashFile(paths, site)) == hash && portListening(port) {
		return port, nil
	}
	Stop(paths, site) // the old config, or a half-dead leftover

	confPath := caddyfilePath(paths, site)
	if err := os.MkdirAll(filepath.Dir(confPath), 0o755); err != nil {
		return 0, err
	}
	// Worker mode doesn't read it (Octane writes its own), but it is
	// still the record of what this site is configured with.
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		return 0, err
	}
	// A server we just stopped can take a moment to let go of the port;
	// anything still on it after that is not ours, and the readiness
	// dial would mistake it for the new server.
	if !waitPortFree(port, 10*time.Second) {
		return 0, fmt.Errorf("port %d for %s's FrankenPHP server is in use by another program", port, site)
	}

	var argv []string
	env := withoutPHPIni(os.Environ())
	// Caddy reads XDG_* first on every OS: give each site its own data
	// and config folders, so this server never touches the main Caddy's
	// (its local CA, autosave.json) or another site's.
	siteDir := filepath.Dir(confPath)
	env = withVar(env, "XDG_DATA_HOME", filepath.Join(siteDir, "data"))
	env = withVar(env, "XDG_CONFIG_HOME", filepath.Join(siteDir, "config"))
	if r.Worker {
		cli := r.PHPCLI
		if len(cli) == 0 {
			cli = []string{r.Bin, "php-cli"}
		}
		argv = append(append([]string{}, cli...), "artisan", "octane:frankenphp",
			"--host=127.0.0.1",
			"--port="+strconv.Itoa(port),
			"--admin-port="+strconv.Itoa(port+1000),
			// Octane may offer to install or download things; nobody
			// can answer that prompt.
			"--no-interaction")
		// Octane looks for `frankenphp` on PATH before downloading its own.
		env = withPathPrefix(env, filepath.Dir(r.Bin))
		keys := make([]string, 0, len(r.Site.Env))
		for k := range r.Site.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+r.Site.Env[k])
		}
	} else {
		argv = []string{r.Bin, "run", "--config", confPath, "--adapter", "caddyfile"}
	}

	logPath := logFilePath(paths, site)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	desc := "frankenphp run"
	if r.Worker {
		desc = "artisan octane:frankenphp"
	}
	fmt.Fprintf(logFile, "\n---- %s: mullion starting `%s` on 127.0.0.1:%d (FrankenPHP %s) ----\n",
		time.Now().Format(time.RFC3339), desc, port, filepath.Base(filepath.Dir(r.Bin)))

	cmd := proc.Quiet(argv[0], argv[1:]...)
	cmd.Dir = r.ProjectDir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// An invisible console, not none: under DETACHED_PROCESS (proc.Detach)
	// every console program it starts would pop a visible console window
	// on Windows.
	proc.DetachHiddenConsole(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s's FrankenPHP server: %w", site, err)
	}
	pid := cmd.Process.Pid
	// Reap it in the background and record how it ended: a server that
	// dies without a word otherwise leaves only the header line.
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		how := "exited (status 0)"
		if err != nil {
			how = err.Error()
		}
		if f, ferr := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			fmt.Fprintf(f, "---- %s: `%s` %s ----\n", time.Now().Format(time.RFC3339), desc, how)
			f.Close()
		}
		close(exited)
	}()
	if err := os.WriteFile(pidFile(paths, site), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		killTree(pid)
		return 0, err
	}

	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			_ = os.Remove(pidFile(paths, site))
			return 0, fmt.Errorf("%s's FrankenPHP server exited — last log lines:\n%s(full log: %s)",
				site, logTail(logPath), logPath)
		default:
		}
		if portListening(port) {
			// Recorded only once it is up: a failed start must not look
			// current to the next Ensure.
			if err := os.WriteFile(hashFile(paths, site), []byte(hash), 0o644); err != nil {
				return 0, err
			}
			return port, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	Stop(paths, site)
	return 0, fmt.Errorf("%s's FrankenPHP server did not open port %d within %s — last log lines:\n%s(full log: %s)",
		site, port, readyTimeout, logTail(logPath), logPath)
}

// configHash identifies what a running server was started with.
func configHash(conf string, worker bool, bin string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00worker=%t\x00%s", conf, worker, bin)
	return hex.EncodeToString(h.Sum(nil))
}

// Stop terminates one site's FrankenPHP server (no-op when not running)
// and waits briefly for it to go.
func Stop(paths pmdir.Paths, site string) {
	if pid := readInt(pidFile(paths, site)); pid > 0 && processAlive(pid) {
		killTree(pid)
		if !waitGone(pid, stopGrace) {
			// It ignored the polite kill: force it, so the pid file never
			// goes while the server still holds the port.
			forceKillTree(pid)
			waitGone(pid, 5*time.Second)
		}
	}
	_ = os.Remove(pidFile(paths, site))
	_ = os.Remove(hashFile(paths, site))
}

// waitGone waits up to timeout for pid to exit and reports whether it did.
func waitGone(pid int, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); processAlive(pid); {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// StopAll terminates every managed FrankenPHP server.
func StopAll(paths pmdir.Paths) {
	entries, err := os.ReadDir(paths.PidsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "franken-") && strings.HasSuffix(name, ".pid") {
			Stop(paths, strings.TrimSuffix(strings.TrimPrefix(name, "franken-"), ".pid"))
		}
	}
}

// AssignPort returns the first FrankenPHP port from 44001 not in used.
func AssignPort(used map[int]bool) int {
	for p := 44001; ; p++ {
		if !used[p] {
			return p
		}
	}
}

// withVar sets key=val in env, replacing any existing value.
func withVar(env []string, key, val string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, key) {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+val)
}

// withPathPrefix puts dir first on PATH (matched case-insensitively:
// Windows spells it Path).
func withPathPrefix(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PATH") {
			if found {
				continue
			}
			found = true
			if v != "" {
				dir += string(os.PathListSeparator) + v
			}
			out = append(out, k+"="+dir)
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}

func waitPortFree(port int, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); ; {
		if !portListening(port) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// portListening dials the loopback port we told the server to bind.
func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func readString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readInt(path string) int {
	n, err := strconv.Atoi(readString(path))
	if err != nil {
		return 0
	}
	return n
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return "  " + strings.Join(lines, "\n  ") + "\n"
}
