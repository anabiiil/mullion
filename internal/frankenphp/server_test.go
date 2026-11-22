//go:build !windows

package frankenphp

import (
	"fmt"
	"net"
	"os"
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
echo "PWD=$(pwd)"
port=""
if [ "$1" = "run" ]; then
  port=$(sed -n 's/^http:\/\/:\([0-9]*\).*/\1/p' "$3")
else
  for a in "$@"; do
    case "$a" in --port=*) port="${a#--port=}" ;; esac
  done
fi
exec python3 -m http.server "$port" --bind 127.0.0.1
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
	t.Cleanup(func() { StopAll(paths) })
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
	pid := readInt(pidFile(paths, site))
	if pid <= 0 {
		t.Fatalf("no pid file for %s", site)
	}
	return pid
}

func readLog(paths pmdir.Paths, site string) string {
	b, _ := os.ReadFile(logFilePath(paths, site))
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
	want := fmt.Sprintf("args: php-cli artisan octane:frankenphp --host=127.0.0.1 --port=%d --admin-port=%d --no-interaction", port, port+1000)
	realProject, _ := filepath.EvalSymlinks(r.ProjectDir)
	for _, w := range []string{
		want,
		"PHPRC=unset PHP_INI_SCAN_DIR=unset APP_ENV=production",
		"PATH=" + filepath.Dir(r.Bin) + string(os.PathListSeparator),
		"PWD=" + realProject,
	} {
		if !strings.Contains(log, w) {
			t.Errorf("log lacks %q:\n%s", w, log)
		}
	}
	if _, err := os.Stat(filepath.Join(paths.FrankenDir(), "sites", "blog", "Caddyfile")); err != nil {
		t.Errorf("worker mode should still write the Caddyfile: %v", err)
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
	for _, f := range []string{pidFile(paths, "blog"), hashFile(paths, "blog")} {
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
