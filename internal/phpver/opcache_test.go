package phpver

import (
	"fmt"
	"net"
	"net/http"
	"net/http/fcgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
)

func opcacheSandbox(t *testing.T, goos, version, ini string, dll bool) (pmdir.Paths, string) {
	t.Helper()
	old := opcacheGOOS
	opcacheGOOS = goos
	t.Cleanup(func() { opcacheGOOS = old })
	paths := pmdir.Paths{Home: t.TempDir()}
	dir := paths.PhpVersionDir(version)
	os.MkdirAll(filepath.Join(dir, "ext"), 0o755)
	if dll {
		os.WriteFile(filepath.Join(dir, "ext", "php_opcache.dll"), []byte("x"), 0o644)
	}
	iniPath := filepath.Join(dir, "php.ini")
	os.WriteFile(iniPath, []byte(ini), 0o644)
	return paths, iniPath
}

func readOpcacheIni(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ptr[T any](v T) *T { return &v }

func TestOpcacheDefaultsAndRead(t *testing.T) {
	paths, _ := opcacheSandbox(t, "darwin", "8.3.10", "display_errors=On\n", false)
	o, err := ReadOpcache(paths, "8.3.10")
	if err != nil {
		t.Fatal(err)
	}
	// PHP < 8.4: jit=tracing but a 0 buffer — effectively off.
	if !o.Enabled || o.CLI || o.JIT != "off" || o.JITRaw != "tracing" || o.JITBuffer != "0" || o.Memory != 128 ||
		o.MaxFiles != 10000 || !o.ValidateTimestamps || o.RevalidateFreq != 2 || !o.Builtin {
		t.Errorf("8.3 defaults = %+v", o)
	}
	paths, _ = opcacheSandbox(t, "darwin", "8.4.1", "opcache.enable=0\nopcache.jit=1205\nopcache.memory_consumption=256 ; MB\n", false)
	o, _ = ReadOpcache(paths, "8.4.1")
	if o.Enabled || o.JIT != "function" || o.JITBuffer != "64M" || o.Memory != 256 {
		t.Errorf("8.4 read = %+v", o)
	}
	if jitMode("1235", "64M") != "custom" || jitMode("tracing", "0") != "off" || jitMode("on", "32M") != "tracing" {
		t.Error("jitMode")
	}
}

func TestSetOpcacheMac(t *testing.T) {
	// A stray zend_extension=opcache (it warns on static builds) is
	// commented out; no line is ever added.
	paths, ini := opcacheSandbox(t, "darwin", "8.3.10", "opcache.enable=1\nzend_extension=opcache\n", false)
	err := SetOpcache(paths, "8.3.10", OpcacheChange{CLI: ptr(true), JIT: ptr("tracing"), ValidateTimestamps: ptr(true), RevalidateFreq: ptr(0), Memory: ptr(192)})
	if err != nil {
		t.Fatal(err)
	}
	got := readOpcacheIni(t, ini)
	for _, want := range []string{"opcache.enable_cli=1\n", "opcache.jit=tracing\n", "opcache.jit_buffer_size=64M\n", "opcache.validate_timestamps=1\n",
		"opcache.revalidate_freq=0\n", "opcache.memory_consumption=192\n", ";zend_extension=opcache\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("php.ini lacks %q:\n%s", want, got)
		}
	}
	o, _ := ReadOpcache(paths, "8.3.10")
	if !o.CLI || o.JIT != "tracing" || o.ExtLine {
		t.Errorf("after = %+v", o)
	}
	// Off writes "disable"; disabling everything adds no zend line.
	if err := SetOpcache(paths, "8.3.10", OpcacheChange{Enabled: ptr(false), CLI: ptr(false), JIT: ptr("off")}); err != nil {
		t.Fatal(err)
	}
	got = readOpcacheIni(t, ini)
	if !strings.Contains(got, "opcache.jit=disable\n") || !strings.Contains(got, "opcache.enable=0\n") || strings.Contains(got, "\nzend_extension=opcache") {
		t.Errorf("off:\n%s", got)
	}
	for _, bad := range []OpcacheChange{{JIT: ptr("fast")}, {Memory: ptr(1)}, {JITBuffer: ptr("lots")}, {RevalidateFreq: ptr(-1)}, {MaxFiles: ptr(10)}} {
		if err := SetOpcache(paths, "8.3.10", bad); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
}

func TestSetOpcacheWindows(t *testing.T) {
	// PHP ≤ 8.4: the DLL needs zend_extension=opcache — the stock
	// php.ini's commented line is re-enabled.
	paths, ini := opcacheSandbox(t, "windows", "8.4.1", ";zend_extension=opcache\nextension_dir = \"ext\"\nopcache.enable=1\n", true)
	o, _ := ReadOpcache(paths, "8.4.1")
	if o.Enabled || o.Builtin || o.ExtLine {
		t.Errorf("no DLL loaded means no OPcache: %+v", o)
	}
	if err := SetOpcache(paths, "8.4.1", OpcacheChange{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	got := readOpcacheIni(t, ini)
	if !strings.HasPrefix(got, "zend_extension=opcache\n") || strings.Count(got, "zend_extension=opcache") != 1 {
		t.Errorf("enable:\n%s", got)
	}
	if o, _ := ReadOpcache(paths, "8.4.1"); !o.Enabled || !o.ExtLine {
		t.Errorf("after enable = %+v", o)
	}
	// Off for sites but on for the CLI keeps the DLL loaded.
	SetOpcache(paths, "8.4.1", OpcacheChange{Enabled: ptr(false), CLI: ptr(true)})
	if !zendOpcacheActive(readOpcacheIni(t, ini)) {
		t.Error("CLI still needs the DLL")
	}
	// Both off: the line is commented out.
	SetOpcache(paths, "8.4.1", OpcacheChange{CLI: ptr(false)})
	got = readOpcacheIni(t, ini)
	if zendOpcacheActive(got) || !strings.Contains(got, ";zend_extension=opcache") {
		t.Errorf("both off:\n%s", got)
	}
	// No line at all: one is appended when enabling.
	paths, ini = opcacheSandbox(t, "windows", "8.4.1", "opcache.enable=0\n", true)
	SetOpcache(paths, "8.4.1", OpcacheChange{Enabled: ptr(true)})
	if got := readOpcacheIni(t, ini); !strings.HasSuffix(got, "zend_extension=opcache\n") {
		t.Errorf("append:\n%q", got)
	}

	// PHP 8.5: built in, no DLL — no zend_extension line, ever.
	paths, ini = opcacheSandbox(t, "windows", "8.5.0", "opcache.enable=0\n", false)
	if err := SetOpcache(paths, "8.5.0", OpcacheChange{Enabled: ptr(true), CLI: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if got := readOpcacheIni(t, ini); strings.Contains(got, "zend_extension") {
		t.Errorf("8.5 got a zend_extension line:\n%s", got)
	}
	if o, _ := ReadOpcache(paths, "8.5.0"); !o.Builtin || !o.Enabled || !strings.Contains(o.ConfigNote, "8.5") {
		t.Errorf("8.5 = %+v", o)
	}
}

func TestDevOpcacheIni(t *testing.T) {
	paths, _ := opcacheSandbox(t, "darwin", "8.4.1", DevOpcacheIni(), false)
	o, _ := ReadOpcache(paths, "8.4.1")
	if !o.Enabled || o.CLI || o.JIT != "off" || o.JITRaw != "disable" || !o.ValidateTimestamps || o.RevalidateFreq != 0 {
		t.Errorf("dev defaults = %+v", o)
	}
}

func TestParseOpcacheStatus(t *testing.T) {
	body := `{"php":"8.4.1","sapi":"fpm-fcgi","loaded":true,"status":{"opcache_enabled":true,"cache_full":false,
"memory_usage":{"used_memory":1000,"free_memory":2000,"wasted_memory":30},
"opcache_statistics":{"num_cached_scripts":12,"num_cached_keys":20,"max_cached_keys":16229,"hits":90,"misses":10,"opcache_hit_rate":90.0,"start_time":1700000000,"oom_restarts":0},
"jit":{"enabled":false,"on":false,"buffer_size":0,"buffer_free":0}}}`
	s, err := parseOpcacheStatus([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.SAPI != "fpm-fcgi" || s.CachedScripts != 12 || s.HitRate != 90 || s.MemoryUsed != 1000 || s.MaxKeys != 16229 {
		t.Errorf("status = %+v", s)
	}
	// opcache_get_status() returns false when OPcache is off.
	s, err = parseOpcacheStatus([]byte(`{"php":"8.4.1","sapi":"cgi-fcgi","loaded":true,"status":false}`))
	if err != nil || s.Enabled || !s.Loaded {
		t.Errorf("disabled = %+v", s)
	}
	if _, err := parseOpcacheStatus([]byte("<b>Fatal error</b>")); err == nil {
		t.Error("garbage should fail")
	}
}

// TestFcgiClient runs the client against the standard library's
// FastCGI server.
func TestFcgiClient(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	big := strings.Repeat("x", 70000) // spans several records
	go fcgi.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := fcgi.ProcessEnv(r)
		switch {
		case strings.HasSuffix(env["SCRIPT_FILENAME"], "fail.php"):
			w.WriteHeader(500)
			fmt.Fprint(w, "boom")
		case strings.HasSuffix(env["SCRIPT_FILENAME"], "big.php"):
			fmt.Fprint(w, big)
		default:
			fmt.Fprintf(w, "%s %s %s", r.Method, env["SCRIPT_FILENAME"], env["SERVER_SOFTWARE"])
		}
	}))
	addr := ln.Addr().String()
	body, err := fcgiGet(addr, "/tmp/x/status.php", 3*time.Second)
	if err != nil || string(body) != "GET /tmp/x/status.php mullion" {
		t.Errorf("body = %q %v", body, err)
	}
	if body, err := fcgiGet(addr, "/tmp/big.php", 3*time.Second); err != nil || len(body) != len(big) {
		t.Errorf("big body: %d bytes, %v", len(body), err)
	}
	if _, err := fcgiGet(addr, "/tmp/fail.php", 3*time.Second); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("a 500 should fail: %v", err)
	}
	if _, err := fcgiGet("127.0.0.1:1", "/x.php", time.Second); err == nil {
		t.Error("nothing listening should fail")
	}
}

// TestLiveOpcacheStatusE2E runs the status script through a REAL
// php-fpm (PHP_FPM_E2E = a php-fpm binary, e.g. a Mullion install's)
// on a scratch port — never the user's running workers.
func TestLiveOpcacheStatusE2E(t *testing.T) {
	fpm := os.Getenv("PHP_FPM_E2E")
	if fpm == "" {
		t.Skip("set PHP_FPM_E2E to a php-fpm binary to run")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "fpm.conf")
	os.WriteFile(conf, []byte("[global]\nerror_log = "+filepath.Join(dir, "fpm.log")+"\ndaemonize = no\n[www]\nlisten = 127.0.0.1:29099\npm = static\npm.max_children = 2\n"), 0o644)
	ini := filepath.Join(dir, "php.ini")
	os.WriteFile(ini, []byte(DevOpcacheIni()), 0o644)
	cmd := exec.Command(fpm, "-n", "-c", ini, "-y", conf)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	script := filepath.Join(dir, "s.php")
	os.WriteFile(script, []byte(statusPHP), 0o644)
	var body []byte
	var err error
	for i := 0; i < 50; i++ {
		if body, err = fcgiGet("127.0.0.1:29099", script, 2*time.Second); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseOpcacheStatus(body)
	if err != nil {
		t.Fatal(err)
	}
	// A second request is counted (a miss: the script is too new to be
	// cached — opcache.file_update_protection).
	body, _ = fcgiGet("127.0.0.1:29099", script, 2*time.Second)
	s2, _ := parseOpcacheStatus(body)
	t.Logf("first: %+v", s)
	t.Logf("second: %+v", s2)
	if !s.Loaded || !s.Enabled || s.SAPI != "fpm-fcgi" || s2.Hits+s2.Misses <= s.Hits+s.Misses || s.MemoryFree == 0 {
		t.Errorf("status not from a live FPM with OPcache: %+v / %+v", s, s2)
	}
}
