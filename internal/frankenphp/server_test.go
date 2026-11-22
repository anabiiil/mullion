//go:build !windows

package frankenphp

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// fakeFranken stands in for the frankenphp binary: `run --config <file>`
// serves the port from the Caddyfile's "http://:PORT" line, and
// `php-cli artisan octane:frankenphp --port=N` serves N. Both print the
// environment variables the tests check first.
const fakeFranken = `#!/bin/sh
echo "args: $*"
echo "PHPRC=${PHPRC-unset} PHP_INI_SCAN_DIR=${PHP_INI_SCAN_DIR-unset} APP_ENV=${APP_ENV-unset}"
echo "PATH=$PATH"
echo "XDG_DATA_HOME=${XDG_DATA_HOME-unset} XDG_CONFIG_HOME=${XDG_CONFIG_HOME-unset}"
echo "PWD=$(pwd)"
[ -n "$FAKE_PIDLOG" ] && echo $$ >> "$FAKE_PIDLOG"
port=""
if [ "$1" = "run" ]; then
  port=$(sed -n 's/^http:\/\/:\([0-9]*\).*/\1/p' "$3")
else
  for a in "$@"; do
    case "$a" in --port=*) port="${a#--port=}" ;; esac
  done
fi
echo "port=$port"
FAKE_FRANKEN_SERVE="$port" exec "$FAKE_SERVER_BIN"
`

func setup(t *testing.T, script string) (pmdir.Paths, Run) {
	t.Helper()
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "frankenphp")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	r := Run{
		Paths:      paths,
		Bin:        bin,
		ProjectDir: project,
		Site:       SiteConf{Name: "blog", Root: filepath.Join(project, "public"), Port: freePort(t)},
	}
	// The fake server's processes are sh and the test binary, not frankenphp.
	oldIs := isFrankenPID
	isFrankenPID = func(int) bool { return true }
	t.Cleanup(func() {
		StopAll(paths)
		isFrankenPID = oldIs
	})
	return paths, r
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func accepts(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

func pidOf(t *testing.T, paths pmdir.Paths, site string) int {
	t.Helper()
	pid := readInt(PidPath(paths, site))
	if pid <= 0 {
		t.Fatalf("no pid file for %s", site)
	}
	return pid
}

func readLog(paths pmdir.Paths, site string) string {
	b, _ := os.ReadFile(LogPath(paths, site))
	return string(b)
}

func TestEnsureStartsAndReportsPort(t *testing.T) {
	t.Setenv("PHPRC", "/somewhere/php.ini")
	t.Setenv("PHP_INI_SCAN_DIR", "/somewhere/conf.d")
	paths, r := setup(t, fakeFranken)

	port, err := Ensure(r)
	if err != nil {
		t.Fatal(err)
	}
	if port != r.Site.Port {
		t.Errorf("port = %d, want %d", port, r.Site.Port)
	}
	if !accepts(port) {
		t.Error("nothing is listening on the port")
	}
	if !Running(paths, "blog") {
		t.Error("Running = false")
	}
	conf, err := os.ReadFile(filepath.Join(paths.FrankenDir(), "sites", "blog", "Caddyfile"))
	if err != nil || string(conf) != Caddyfile(r.Site) {
		t.Errorf("Caddyfile not written as rendered: %v\n%s", err, conf)
	}
	if _, err := os.Stat(hashFile(paths, "blog")); err != nil {
		t.Errorf("no hash file: %v", err)
	}
	log := readLog(paths, "blog")
	for _, want := range []string{
		"mullion starting",
		"args: run --config " + filepath.Join(paths.FrankenDir(), "sites", "blog", "Caddyfile") + " --adapter caddyfile",
		"PHPRC=unset PHP_INI_SCAN_DIR=unset",
		"XDG_DATA_HOME=" + filepath.Join(paths.FrankenDir(), "sites", "blog", "data") +
			" XDG_CONFIG_HOME=" + filepath.Join(paths.FrankenDir(), "sites", "blog", "config"),
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestEnsureRestartsOnlyOnChange(t *testing.T) {
	paths, r := setup(t, fakeFranken)
	if _, err := Ensure(r); err != nil {
		t.Fatal(err)
	}
	first := pidOf(t, paths, "blog")

	if _, err := Ensure(r); err != nil {
		t.Fatal(err)
	}
	if got := pidOf(t, paths, "blog"); got != first {
		t.Fatalf("unchanged Run restarted the server: pid %d → %d", first, got)
	}

	r.Site.Env = map[string]string{"APP_ENV": "production"}
	if _, err := Ensure(r); err != nil {
		t.Fatal(err)
	}
	// Classic mode: Env reaches PHP through the Caddyfile's php_server
	// env lines, which is what changes the hash.
	conf, _ := os.ReadFile(filepath.Join(paths.FrankenDir(), "sites", "blog", "Caddyfile"))
	if !strings.Contains(string(conf), `env APP_ENV "production"`) {
		t.Errorf("Env not in the classic-mode Caddyfile:\n%s", conf)
	}
	second := pidOf(t, paths, "blog")
	if second == first {
		t.Fatal("changed Env did not restart the server")
	}
	if processAlive(first) {
		t.Errorf("old server (pid %d) is still alive", first)
	}
	if !accepts(r.Site.Port) {
		t.Error("restarted server is not listening")
	}
}

func TestEnsureWorkerMode(t *testing.T) {
	t.Setenv("PHPRC", "/somewhere/php.ini")
	paths, r := setup(t, fakeFranken)
	r.Worker = true
	r.PHPCLI = []string{r.Bin, "php-cli"}
	r.Site.Env = map[string]string{"APP_ENV": "production"}

	port, err := Ensure(r)
	if err != nil {
		t.Fatal(err)
	}
	if !accepts(port) {
		t.Error("nothing is listening on the port")
	}
	log := readLog(paths, "blog")
	confPath := filepath.Join(paths.FrankenDir(), "sites", "blog", "Caddyfile")
	want := fmt.Sprintf("args: php-cli artisan octane:frankenphp --caddyfile=%s --host=127.0.0.1 --port=%d --admin-host=127.0.0.1 --admin-port=%d --no-interaction", confPath, port, port+1000)
	realProject, _ := filepath.EvalSymlinks(r.ProjectDir)
	for _, w := range []string{
		want,
		"PHPRC=unset PHP_INI_SCAN_DIR=unset APP_ENV=production",
		"PATH=" + filepath.Dir(r.Bin) + string(os.PathListSeparator),
		"PWD=" + realProject,
		"XDG_DATA_HOME=" + filepath.Join(paths.FrankenDir(), "sites", "blog", "data") +
			" XDG_CONFIG_HOME=" + filepath.Join(paths.FrankenDir(), "sites", "blog", "config"),
	} {
		if !strings.Contains(log, w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
	// Octane is handed our worker Caddyfile, not its own stub.
	if conf, err := os.ReadFile(confPath); err != nil || string(conf) != WorkerCaddyfile(r.Site) {
		t.Errorf("worker Caddyfile not written as rendered: %v\n%s", err, conf)
	}
}

func TestStopKillsServer(t *testing.T) {
	paths, r := setup(t, fakeFranken)
	if _, err := Ensure(r); err != nil {
		t.Fatal(err)
	}
	pid := pidOf(t, paths, "blog")
	Stop(paths, "blog")
	if accepts(r.Site.Port) {
		t.Error("port still accepts connections after Stop")
	}
	if processAlive(pid) {
		t.Error("process still alive after Stop")
	}
	for _, f := range []string{PidPath(paths, "blog"), hashFile(paths, "blog")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still exists", f)
		}
	}
	if Running(paths, "blog") {
		t.Error("Running = true after Stop")
	}
}

func TestEnsureFailureLandsInLog(t *testing.T) {
	paths, r := setup(t, "#!/bin/sh\necho 'Error: adapting config: boom' >&2\nexit 1\n")
	_, err := Ensure(r)
	if err == nil {
		t.Fatal("a binary that exits 1 should make Ensure fail")
	}
	if !strings.Contains(err.Error(), "adapting config: boom") {
		t.Errorf("error lacks the log tail: %v", err)
	}
	log := readLog(paths, "blog")
	if !strings.Contains(log, "mullion:") {
		t.Errorf("failure reason not in the log:\n%s", log)
	}
	if _, err := os.Stat(hashFile(paths, "blog")); !os.IsNotExist(err) {
		t.Error("a failed start must not record a hash (the next Ensure would skip it)")
	}
}

func TestAssignPort(t *testing.T) {
	if got := AssignPort(nil); got != 44001 {
		t.Errorf("AssignPort(nil) = %d", got)
	}
	if got := AssignPort(map[int]bool{44001: true, 44002: true, 44004: true}); got != 44003 {
		t.Errorf("AssignPort = %d, want 44003", got)
	}
}

// A server that ignores SIGTERM is killed outright once the grace period
// runs out, before its pid file goes.
func TestStopEscalatesToSIGKILL(t *testing.T) {
	old := stopGrace
	stopGrace = 500 * time.Millisecond
	t.Cleanup(func() { stopGrace = old })
	stubborn := strings.Replace(fakeFranken, "#!/bin/sh\n", "#!/bin/sh\ntrap '' TERM\n", 1)
	paths, r := setup(t, stubborn)
	if _, err := Ensure(r); err != nil {
		t.Fatal(err)
	}
	pid := pidOf(t, paths, "blog")
	Stop(paths, "blog")
	if processAlive(pid) {
		t.Errorf("pid %d survived Stop", pid)
	}
	if accepts(r.Site.Port) {
		t.Error("port still accepts connections after Stop")
	}
}

// Two Mullion processes applying at once (goroutines here: the lock is a
// file) must leave exactly one server, the one in the pid file.
func TestConcurrentEnsureLeavesOneServer(t *testing.T) {
	pidlog := filepath.Join(t.TempDir(), "pids")
	t.Setenv("FAKE_PIDLOG", pidlog)
	paths, r := setup(t, fakeFranken)
	if _, err := Ensure(r); err != nil {
		t.Fatal(err)
	}
	r.Site.Env = map[string]string{"APP_ENV": "production"}

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := Ensure(r)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("Ensure: %v", err)
		}
	}

	live := pidOf(t, paths, "blog")
	if !processAlive(live) || !accepts(r.Site.Port) {
		t.Fatalf("pid file's server %d is not up", live)
	}
	data, _ := os.ReadFile(pidlog)
	started := strings.Fields(string(data))
	// The first server, then one restart for the changed config.
	if len(started) != 2 {
		t.Errorf("servers started = %v, want 2", started)
	}
	for _, f := range started {
		pid, _ := strconv.Atoi(f)
		if pid != live && processAlive(pid) {
			t.Errorf("orphan server pid %d still alive (pid file holds %d)", pid, live)
		}
	}
	if _, err := os.Stat(lockPath(paths, "blog")); !os.IsNotExist(err) {
		t.Errorf("lock file left behind: %v", err)
	}
}

func TestLockBreaksStale(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	path := lockPath(paths, "blog")

	// Held by a live process (us) and fresh: not taken.
	held, err := lockSite(paths, "blog")
	if err != nil {
		t.Fatal(err)
	}
	breakStaleLock(path)
	if readString(path) != held.token {
		t.Fatal("a fresh lock held by a live process was broken")
	}
	held.unlock()

	// Owner dead: broken at once.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(strconv.Itoa(dead.Process.Pid)+" abc"), 0o644)
	start := time.Now()
	l, err := lockSite(paths, "blog")
	if err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("dead owner's lock not broken: %v", err)
	}
	l.unlock()

	// Owner alive but too old: broken.
	os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+" abc"), 0o644)
	old := time.Now().Add(-lockStale - time.Minute)
	os.Chtimes(path, old, old)
	if l, err = lockSite(paths, "blog"); err != nil {
		t.Fatal(err)
	}
	l.unlock()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("unlock left the lock file")
	}
}

// A pid file that outlived a reboot may name an unrelated process: Stop
// leaves it alone and only drops the pid file.
func TestStopSparesForeignProcess(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	pid := other.Process.Pid
	os.WriteFile(PidPath(paths, "blog"), []byte(strconv.Itoa(pid)), 0o644)

	Stop(paths, "blog")
	if !processAlive(pid) {
		t.Error("Stop killed a process that is not frankenphp")
	}
	if _, err := os.Stat(PidPath(paths, "blog")); !os.IsNotExist(err) {
		t.Error("pid file not removed")
	}
}
