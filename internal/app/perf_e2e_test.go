package app

// End-to-end check of the Performance tab against a real Laravel app,
// php-fpm and Caddy — opt-in, it needs a prepared project:
//
//	MULLION_PERF_E2E=<laravel app dir>   (its .env points at a database)
//	MULLION_PERF_E2E_PHP=<php version dir: php, php-fpm, php.ini>
//	MULLION_PERF_E2E_CADDY=<caddy binary>
//
// Everything runs in a sandbox home on ports 18080 (Caddy, admin off)
// and 19084 (php-fpm); Apply is never called, so /etc/hosts and the
// real Caddy are left alone.

import (
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
	"strings"
	"syscall"
	"testing"
	"time"

	"pm/internal/config"
	"pm/internal/loadtest"
	"pm/internal/perf"
	"pm/internal/pmdir"
)

func TestPerfE2E(t *testing.T) {
	proj, phpDir, caddyBin := os.Getenv("MULLION_PERF_E2E"), os.Getenv("MULLION_PERF_E2E_PHP"), os.Getenv("MULLION_PERF_E2E_CADDY")
	if proj == "" || phpDir == "" || caddyBin == "" || runtime.GOOS == "windows" {
		t.Skip("set MULLION_PERF_E2E, MULLION_PERF_E2E_PHP and MULLION_PERF_E2E_CADDY")
	}
	for _, addr := range []string{"127.0.0.1:18080", "127.0.0.1:19084"} {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			t.Fatalf("%s is taken", addr)
		}
	}
	a := sandboxApp(t)
	work := t.TempDir()
	// MULLION_PERF_E2E_HOME keeps the sandbox (for a look in the panel).
	if keep := os.Getenv("MULLION_PERF_E2E_HOME"); keep != "" {
		os.RemoveAll(keep)
		t.Setenv("HOME", keep)
		a.Paths = pmdir.Paths{Home: filepath.Join(keep, ".mullion")}
		if err := a.Paths.EnsureLayout(); err != nil {
			t.Fatal(err)
		}
		st, err := config.Load(a.Paths)
		if err != nil {
			t.Fatal(err)
		}
		a.State = st
		work = filepath.Join(keep, "work")
		os.MkdirAll(work, 0o755)
	}
	const version = "8.4.23"
	os.MkdirAll(a.Paths.PhpDir(), 0o755)
	if err := os.Symlink(phpDir, a.Paths.PhpVersionDir(version)); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(a.Paths.BinDir(), 0o755)
	os.Symlink(caddyBin, a.Paths.CaddyExe())
	a.State.Config.GlobalPHP = version
	a.State.Config.TLD = "test"
	a.State.AddSite(config.Site{Name: "pfapp", Path: proj, DevPort: 18080})
	if err := a.SetWatch("pfapp", config.Watch{Enabled: true, Dumps: true, Deep: true}); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(proj, "bootstrap", "cache", "config.php"))

	// php-fpm on 19084.
	fpmConf := filepath.Join(work, "fpm.conf")
	os.WriteFile(fpmConf, []byte(fmt.Sprintf("[global]\nerror_log = %s/fpm.log\ndaemonize = no\n[www]\nlisten = 127.0.0.1:19084\npm = static\npm.max_children = 8\nclear_env = no\n", work)), 0o644)
	var fpm, cad *exec.Cmd
	startFPM := func() {
		fpm = exec.Command(filepath.Join(phpDir, "php-fpm"), "-c", filepath.Join(phpDir, "php.ini"), "-y", fpmConf, "-p", work, "-F")
		fpm.Stdout, fpm.Stderr = io.Discard, io.Discard
		if err := fpm.Start(); err != nil {
			t.Fatal(err)
		}
		waitPort(t, "127.0.0.1:19084")
	}
	stop := func(c *exec.Cmd) {
		if c != nil && c.Process != nil {
			// SIGTERM: php-fpm's master takes its children down with it.
			c.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { c.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				c.Process.Kill()
				<-done
			}
		}
	}
	defer func() { stop(cad); stop(fpm) }()

	fcgiRe := regexp.MustCompile(`127\.0\.0\.1:\d+`)
	startCaddy := func() {
		stop(cad)
		if err := a.WriteCaddyfile(nil); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(a.Paths.Caddyfile())
		c := strings.Replace(string(b), "admin 127.0.0.1:2019", "admin off\n\tauto_https off\n\tpersist_config off", 1)
		c = strings.Replace(c, "http://pfapp.test {", "http://localhost:18080 {", 1)
		c = fcgiRe.ReplaceAllStringFunc(c, func(s string) string { return "127.0.0.1:19084" })
		f := filepath.Join(work, "Caddyfile")
		os.WriteFile(f, []byte(c), 0o644)
		cad = exec.Command(caddyBin, "run", "--config", f, "--adapter", "caddyfile")
		cad.Env = append(os.Environ(), "HOME="+work, "XDG_DATA_HOME="+work, "XDG_CONFIG_HOME="+work)
		out, _ := os.Create(filepath.Join(work, "caddy.log"))
		cad.Stdout, cad.Stderr = out, out
		if err := cad.Start(); err != nil {
			t.Fatal(err)
		}
		if !portUp("127.0.0.1:18080") {
			logb, _ := os.ReadFile(filepath.Join(work, "caddy.log"))
			t.Fatalf("caddy didn't start:\n%s\n%s", c, logb)
		}
	}
	restartFPM := func() { stop(fpm); startFPM() }
	startFPM()
	startCaddy()
	t.Log("servers up")

	get := func(path string) map[string]any {
		cl := &http.Client{Timeout: 20 * time.Second}
		res, err := cl.Get("http://localhost:18080" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		body, _ := io.ReadAll(res.Body)
		_ = json.Unmarshal(body, &m)
		if res.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", path, res.StatusCode, body)
		}
		return m
	}
	for i := 1; i <= 5; i++ {
		get(fmt.Sprintf("/posts/%d", i))
		get(fmt.Sprintf("/categories/%d", i))
		get("/fast")
	}
	t.Logf("dev /debug: %v", get("/debug"))

	load := func(label string) perf.LoadResult {
		in := LoadInput{URL: "http://localhost:18080/posts", Concurrency: 10, DurationS: 10, WarmUpS: 1}
		c, err := a.LoadConfig("pfapp", in)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := loadtest.Run(context.Background(), c, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := a.SaveLoad("pfapp", in, c, snap)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("load %s: %d requests in %.1fs, %.1f rps, p50 %.1f p95 %.1f p99 %.1f ms, status %v, errors %d", label, snap.Requests, snap.Elapsed, snap.RPS, snap.P50Ms, snap.P95Ms, snap.P99Ms, snap.Status, snap.Errors)
		if snap.Requests == 0 || snap.Status["200"] != snap.Requests {
			t.Fatalf("%s: %+v", label, snap.Status)
		}
		return r
	}
	dev := load("dev")
	get("/heavy")
	for i := 1; i <= 3; i++ {
		get(fmt.Sprintf("/posts/%d", i))
	}
	time.Sleep(time.Second)

	// Routes.
	res, err := a.PerfRoutes("pfapp")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("routes: %d requests, %d deep", res.Requests, res.Deep)
	byKey := map[string]perf.RouteStat{}
	for _, r := range res.Routes {
		byKey[r.Key] = r
		t.Logf("  %-28s n=%-5d avg %7.1f p50 %7.1f p95 %7.1f p99 %7.1f ms  q %.1f (max %d)  mem %.1f MB  err %.0f%%  n+1 %d  over %v",
			r.Key, r.Count, r.AvgMs, r.P50Ms, r.P95Ms, r.P99Ms, r.AvgQueries, r.MaxQueries, float64(r.PeakMemory)/(1<<20), r.ErrorRate*100, r.NPlusOne, r.Over)
	}
	posts := byKey["GET /posts"]
	if posts.Count < 50 || posts.DeepCount == 0 || posts.AvgQueries < 30 || posts.NPlusOne == 0 || posts.Name != "posts.index" || len(posts.Over) == 0 {
		t.Errorf("/posts row: %+v", posts)
	}
	if r := byKey["GET /posts/{post}"]; r.Count != 8 || r.Name != "posts.show" {
		t.Errorf("/posts/{post}: %+v", r)
	}
	if r := byKey["GET /heavy"]; r.PeakMemory < 80<<20 || len(r.Over) == 0 {
		t.Errorf("/heavy: %+v", r)
	}

	// Readiness.
	rd, err := a.CheckReadiness(context.Background(), "pfapp")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]perf.Finding{}
	for _, f := range rd.Findings {
		ids[f.ID] = f
		t.Logf("  [%s] %s — %s", f.Severity, f.Title, f.ID)
	}
	t.Logf("readiness: %d findings in %d ms; schema %+v; notes %v", len(rd.Findings), rd.Ms, rd.Schema, rd.Notes)
	for _, id := range []string{"laravel.app-debug", "laravel.env-outside-config", "laravel.route-closures", "db.id-index:posts", "db.no-pk:audit_logs", "laravel.strict-models"} {
		if _, ok := ids[id]; !ok {
			t.Errorf("missing finding %s", id)
		}
	}
	n1 := false
	for id := range ids {
		n1 = n1 || strings.HasPrefix(id, "queries.n+1:")
	}
	if !n1 {
		t.Error("no N+1 finding from the Watch data")
	}

	// With .env.production present APP_ENV stays out (Laravel would load
	// that file instead of .env); without it, prod mode sets it.
	if st, _ := a.ProdModeStatus("pfapp"); st.Env["APP_ENV"] != "" || len(st.Notes) == 0 {
		t.Errorf("APP_ENV must stay out with .env.production: %+v", st)
	}
	envProd := filepath.Join(proj, ".env.production")
	if b, err := os.ReadFile(envProd); err == nil {
		os.Remove(envProd)
		defer os.WriteFile(envProd, b, 0o644)
	}

	// Production-like mode, with artisan optimize.
	if err := a.SetProdMode(context.Background(), "pfapp", true, true, func(b []byte) { t.Logf("prod: %s", strings.TrimSpace(string(b))) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, "bootstrap", "cache", "config.php")); err != nil {
		t.Errorf("optimize didn't cache the config: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(proj, "bootstrap", "cache", "config.php"))
	if !strings.Contains(string(cfg), "'debug' => false") {
		t.Error("the cached config must have debug=false (optimize ran with the production env)")
	}
	restartFPM()
	startCaddy()
	d := get("/debug")
	t.Logf("prod /debug: %v", d)
	if d["debug"] != false || d["env"] != "production" || d["vt"] != "0" {
		t.Errorf("prod mode not in effect: %v", d)
	}
	prod := load("prod")
	cmp := perf.CompareLoad(prod, dev)
	t.Logf("prod vs dev: rps %+.1f%%, p50 %+.1f%%, p95 %+.1f%%", cmp.RPSPct, cmp.P50Pct, cmp.P95Pct)

	if err := a.SetProdMode(context.Background(), "pfapp", false, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, "bootstrap", "cache", "config.php")); err == nil {
		t.Error("optimize:clear didn't run")
	}
	restartFPM()
	startCaddy()
	d = get("/debug")
	t.Logf("after off /debug: %v", d)
	if d["debug"] != true || d["env"] != "local" || d["vt"] != "1" {
		t.Errorf("prod mode not undone: %v", d)
	}
}

func waitPort(t *testing.T, addr string) {
	t.Helper()
	if !portUp(addr) {
		t.Fatalf("%s never came up", addr)
	}
}

func portUp(addr string) bool {
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
