// Package redis installs and runs Redis for Mullion — the same way the
// other engines run: one folder per version, one detached process, a
// pid file, and a port probe — plus a small RESP client (resp.go) the
// panel and CLI use to look inside it, and the Laravel .env helpers of
// the project page's Cache tab (env.go).
//
// Where the server comes from:
//
//   - macOS (and Linux): built from the official source tarball
//     (download.redis.io, checked against the SHA-256 published in
//     github.com/redis/redis-hashes) with the system compiler — on macOS
//     the Xcode Command Line Tools. Only the core (redis-server,
//     redis-cli) is built: no TLS, no modules, no extra dependencies.
//     Building happens on the user's machine at their request; Mullion
//     redistributes nothing.
//   - Windows: Redis has no official Windows build. Mullion downloads
//     the portable MSYS2 build from github.com/redis-windows/redis-windows
//     (compiled from the official sources, a plain zip: no installer,
//     no service, no administrator rights).
//
// Layout (under paths.Home/redis):
//
//	<version>/bin/redis-server   the binaries (+ the MSYS2 DLLs on Windows)
//	redis.conf                   rendered on every start (conf.go)
//	data/dump.rdb                the data (kept by `uninstall --keep-data`)
//
// It does not import app: app wires it into Apply, the ports, backups
// and doctor (internal/app/redis.go).
package redis

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/sysproc"
)

// DefaultPort is Redis's own default port.
const DefaultPort = 6379

// Port is where Mullion's Redis listens (127.0.0.1 only): DefaultPort
// unless the user moved it (config redisPort). app.ApplyPortConfig sets
// it at startup.
var Port = DefaultPort

// Password is requirepass ("" = none); app.ApplyPortConfig sets it.
var Password = ""

// ErrNotInstalled is returned when no Redis version is installed.
var ErrNotInstalled = errors.New("Redis is not installed (run: mullion redis install)")

// Label renders a version for humans: "Redis 8.10.2".
func Label(version string) string { return "Redis " + version }

// BaseDir holds every installed version, the config and the data.
func BaseDir(paths pmdir.Paths) string { return paths.RedisDir() }

func versionDir(paths pmdir.Paths, version string) string {
	return filepath.Join(BaseDir(paths), version)
}

// ServerExe is an installed version's redis-server.
func ServerExe(paths pmdir.Paths, version string) string {
	return filepath.Join(versionDir(paths, version), "bin", pmdir.ExeName("redis-server"))
}

// CliExe is an installed version's redis-cli.
func CliExe(paths pmdir.Paths, version string) string {
	return filepath.Join(versionDir(paths, version), "bin", pmdir.ExeName("redis-cli"))
}

// ConfFile is the rendered redis.conf.
func ConfFile(paths pmdir.Paths) string { return filepath.Join(BaseDir(paths), "redis.conf") }

// DataDir holds dump.rdb.
func DataDir(paths pmdir.Paths) string { return filepath.Join(BaseDir(paths), "data") }

// DumpFile is the RDB snapshot.
func DumpFile(paths pmdir.Paths) string { return filepath.Join(DataDir(paths), "dump.rdb") }

// LogFile is where the server's output goes.
func LogFile(paths pmdir.Paths) string { return filepath.Join(paths.LogsDir(), "redis.log") }

// BuildLog is where the macOS/Linux build's compiler output goes.
func BuildLog(paths pmdir.Paths) string { return filepath.Join(paths.LogsDir(), "redis-build.log") }

func pidFile(paths pmdir.Paths) string { return filepath.Join(paths.PidsDir(), "redis.pid") }

// Addr is the server's address.
func Addr() string { return "127.0.0.1:" + strconv.Itoa(Port) }

var fullVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Installed lists the versions present on disk, newest first.
func Installed(paths pmdir.Paths) []string {
	entries, err := os.ReadDir(BaseDir(paths))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !fullVersionRe.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(ServerExe(paths, e.Name())); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return Newer(out[i], out[j]) })
	return out
}

// Progress reports what an install is doing ("Downloading…", "Building…").
type Progress func(status string)

// RemoveVersion deletes one installed version's folder.
func RemoveVersion(paths pmdir.Paths, version string) error {
	if !fullVersionRe.MatchString(version) {
		return fmt.Errorf("invalid Redis version %q", version)
	}
	return os.RemoveAll(versionDir(paths, version))
}

// Remove deletes every installed version and the config, and the data
// too unless keepData. Stopping the server first is the caller's job.
func Remove(paths pmdir.Paths, keepData bool) error {
	if !keepData {
		return os.RemoveAll(BaseDir(paths))
	}
	entries, err := os.ReadDir(BaseDir(paths))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.Name() == "data" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(BaseDir(paths), e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// WriteConfig renders redis.conf for the current Port/Password.
func WriteConfig(paths pmdir.Paths) error {
	if err := os.MkdirAll(DataDir(paths), 0o755); err != nil {
		return err
	}
	return os.WriteFile(ConfFile(paths), []byte(RenderConfig(ConfOptions{Port: Port, Password: Password})), 0o644)
}

// portUp reports whether something accepts connections on the port.
func portUp(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Running reports whether something serves Redis's port.
func Running() bool { return portUp(Port) }

// ourPids lists the redis-server Mullion started: a process running
// from Mullion's folder — never someone else's Redis (Homebrew,
// Docker, Memurai), which must not be stopped.
var ourPids = func(paths pmdir.Paths) []int {
	return sysproc.ProcessesUnder(BaseDir(paths), pmdir.ExeName("redis-server"))
}

// Owned reports whether Mullion's own Redis is running.
func Owned(paths pmdir.Paths) bool { return len(ourPids(paths)) > 0 }

// ownsPort reports whether the process listening on Port is ours — the
// check before sending it SHUTDOWN.
func ownsPort(paths pmdir.Paths) bool {
	pid, _ := sysproc.PortOwner(Port)
	if pid <= 0 {
		return false
	}
	for _, p := range ourPids(paths) {
		if p == pid {
			return true
		}
	}
	return false
}

// Connect opens a client to Mullion's Redis (authenticating when a
// password is set).
func Connect(timeout time.Duration) (*Client, error) {
	c, err := Dial(Addr(), timeout)
	if err != nil {
		return nil, err
	}
	if Password != "" {
		if err := c.Auth(Password); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// Start launches redis-server detached and waits until it answers PING.
// A port already held by something that isn't Mullion's Redis is
// reported instead of fought.
func Start(paths pmdir.Paths, version string) error {
	if Owned(paths) {
		if Running() && ownsPort(paths) {
			return nil
		}
		// Ours, but not answering where it should (still on an old
		// port, or wedged): replace it.
		if err := Stop(paths); err != nil {
			return err
		}
	}
	exe := ServerExe(paths, version)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("%s is not installed (run: mullion redis install)", Label(version))
	}
	if portUp(Port) {
		who := "another program"
		if pid, name := sysproc.PortOwner(Port); pid > 0 {
			who = fmt.Sprintf("%s (pid %d)", name, pid)
		}
		return fmt.Errorf("port %d is already used by %s — stop it, or move Redis to another port (Ports page, or `mullion ports`)", Port, who)
	}
	if err := WriteConfig(paths); err != nil {
		return err
	}
	for _, dir := range []string{paths.LogsDir(), paths.PidsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	out, err := os.OpenFile(LogFile(paths), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	// Relative config path + working directory = no native paths on the
	// command line (see RenderConfig).
	cmd := proc.Quiet(exe, "redis.conf")
	cmd.Dir = BaseDir(paths)
	cmd.Stdout = out
	cmd.Stderr = out
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting Redis: %w", err)
	}
	if err := os.WriteFile(pidFile(paths), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for i := 0; i < 80; i++ {
		if pingOK() {
			return nil
		}
		select {
		case err := <-exited:
			os.Remove(pidFile(paths))
			return fmt.Errorf("Redis exited during startup (%v)%s — see %s", err, lastLogLine(paths), LogFile(paths))
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("Redis did not start listening on port %d (see %s)", Port, LogFile(paths))
}

// pingOK reports whether the server answers PING (it refuses commands
// with -LOADING while it reads a big dump.rdb).
func pingOK() bool {
	c, err := Connect(time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	return c.Ping() == nil
}

// lastLogLine is the newest warning-ish line of the log ("# ..." lines
// are Redis's warnings), for startup failures.
func lastLogLine(paths pmdir.Paths) string {
	data, err := os.ReadFile(LogFile(paths))
	if err != nil {
		return ""
	}
	if len(data) > 64<<10 {
		data = data[len(data)-64<<10:]
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-20; i-- {
		l := lines[i]
		if strings.Contains(l, " # ") || strings.Contains(l, "FATAL") || strings.Contains(l, "Bad directive") {
			return ": " + strings.TrimSpace(l)
		}
	}
	return ""
}

// Stop shuts Mullion's Redis down cleanly — SHUTDOWN SAVE, so the data
// is snapshotted first — then falls back to SIGTERM (which also saves)
// and a hard kill. Not running is not an error.
func Stop(paths pmdir.Paths) error {
	defer os.Remove(pidFile(paths))
	if len(ourPids(paths)) == 0 {
		return nil
	}
	if ownsPort(paths) {
		if c, err := Connect(3 * time.Second); err == nil {
			_ = c.Shutdown()
			c.Close()
		}
		if waitStopped(paths, 15*time.Second) {
			return nil
		}
	}
	for _, pid := range ourPids(paths) {
		if p, err := os.FindProcess(pid); err == nil {
			if runtime.GOOS == "windows" || p.Signal(syscall.SIGTERM) != nil {
				_ = p.Kill()
			}
		}
	}
	if waitStopped(paths, 10*time.Second) {
		return nil
	}
	for _, pid := range ourPids(paths) {
		sysproc.KillProcess(pid)
	}
	if !waitStopped(paths, 5*time.Second) {
		return fmt.Errorf("Redis is still running")
	}
	return nil
}

func waitStopped(paths pmdir.Paths, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(ourPids(paths)) == 0 {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return len(ourPids(paths)) == 0
}

// Newer reports whether version a is newer than b ("8.10.2" > "8.8.3").
func Newer(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}

// copyFile copies src to dst with mode.
func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}
