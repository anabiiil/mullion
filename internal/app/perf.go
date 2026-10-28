package app

// The project page's Performance tab (and `mullion perf`): route stats
// from the Watch data, the production-readiness audit, load tests
// against the site's own hosts, production-like mode and worker health.
// The rules and stores live in internal/perf; this file gathers the
// facts they need from the project, the site's PHP and Caddy.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/detect"
	"pm/internal/devserver"
	"pm/internal/fcgi"
	"pm/internal/loadtest"
	"pm/internal/perf"
	"pm/internal/phpver"
	"pm/internal/redis"
	"pm/internal/watch"
	"pm/internal/watch/explain"
	"pm/internal/workers"
)

// ── settings ─────────────────────────────────────────────────────────

// PerfBudgets is a site's route budgets with defaults filled in.
func PerfBudgets(s config.Site) perf.Budgets {
	var b perf.Budgets
	if p := s.Perf; p != nil {
		b = perf.Budgets{MaxMs: p.MaxMs, MaxQueries: p.MaxQueries, MaxMemoryMB: p.MaxMemoryMB}
	}
	return b.WithDefaults()
}

func ensurePerf(s *config.Site) *config.Perf {
	if s.Perf == nil {
		s.Perf = &config.Perf{}
	}
	return s.Perf
}

// SetPerfBudgets saves a site's route budgets (0 = the default).
func (a *App) SetPerfBudgets(site string, b perf.Budgets) error {
	s, err := a.siteOrErr(site)
	if err != nil {
		return err
	}
	for _, v := range []int{b.MaxMs, b.MaxQueries, b.MaxMemoryMB} {
		if v < 0 || v > 1000000 {
			return errors.New("a budget must be between 0 (the default) and 1000000")
		}
	}
	p := ensurePerf(s)
	p.MaxMs, p.MaxQueries, p.MaxMemoryMB = b.MaxMs, b.MaxQueries, b.MaxMemoryMB
	return a.State.Save()
}

// SetPerfIgnored adds (or with on=false removes) a readiness finding id
// from the site's ignore list.
func (a *App) SetPerfIgnored(site, id string, on bool) error {
	s, err := a.siteOrErr(site)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 300 {
		return errors.New("no finding id")
	}
	p := ensurePerf(s)
	out := p.Ignored[:0:0]
	for _, x := range p.Ignored {
		if x != id {
			out = append(out, x)
		}
	}
	if on {
		out = append(out, id)
	}
	p.Ignored = out
	return a.State.Save()
}

// ── routes ───────────────────────────────────────────────────────────

// PerfRoutesResult is the Routes sub-tab.
type PerfRoutesResult struct {
	Routes   []perf.RouteStat `json:"routes"`
	Requests int              `json:"requests"`
	Deep     int              `json:"deep"` // requests with query data
	Budgets  perf.Budgets     `json:"budgets"`
	// Watch: the watcher's state, so the panel can say what to turn on.
	WatchOn   bool    `json:"watchOn"`
	WatchDeep bool    `json:"watchDeep"`
	ResetAt   float64 `json:"resetAt"`
	Since     float64 `json:"since"` // oldest request counted
	Prod      bool    `json:"prod"`
}

// perfDepth is how many requests back the route table looks.
const perfDepth = 5000

// PerfRequests reads the site's recent requests from the Watch data.
func (a *App) PerfRequests(site string) ([]perf.Request, []watch.Item, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return nil, nil, err
	}
	items := watch.NewFeed(a.Paths, *s, a.State.Host(*s)).Recent(perfDepth)
	reqs := perf.Since(perf.RequestsFromItems(items), perf.ResetAt(a.Paths, s.Name))
	return reqs, items, nil
}

// PerfRoutes aggregates the site's recent requests per route.
func (a *App) PerfRoutes(site string) (PerfRoutesResult, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return PerfRoutesResult{}, err
	}
	reqs, _, err := a.PerfRequests(site)
	if err != nil {
		return PerfRoutesResult{}, err
	}
	st := watch.SettingsOf(*s)
	res := PerfRoutesResult{Budgets: PerfBudgets(*s), Requests: len(reqs), WatchOn: st.Enabled, WatchDeep: st.Enabled && st.Deep,
		ResetAt: perf.ResetAt(a.Paths, s.Name), Prod: s.ProdMode()}
	for _, r := range reqs {
		if r.Deep {
			res.Deep++
		}
	}
	if len(reqs) > 0 {
		res.Since = reqs[0].T
	}
	res.Routes = perf.Aggregate(reqs, res.Budgets)
	return res, nil
}

// PerfResetRoutes starts the route stats over.
func (a *App) PerfResetRoutes(site string) error {
	s, err := a.siteOrErr(site)
	if err != nil {
		return err
	}
	return perf.Reset(a.Paths, s.Name)
}

// ── readiness ────────────────────────────────────────────────────────

// Readiness is the result of a readiness check.
type Readiness struct {
	Site     string            `json:"site"`
	At       time.Time         `json:"at"`
	Ms       int64             `json:"ms"`
	Findings []perf.Finding    `json:"findings"`
	Build    *perf.BuildReport `json:"build,omitempty"`
	Schema   *SchemaSummary    `json:"schema,omitempty"`
	Notes    []string          `json:"notes"`
}

// SchemaSummary describes the database the check read.
type SchemaSummary struct {
	Engine string `json:"engine"`
	Conn   string `json:"conn"`
	Tables int    `json:"tables"`
	FKs    int    `json:"foreignKeys"`
}

func readinessPath(a *App, site string) string {
	return filepath.Join(perf.Dir(a.Paths, site), "readiness.json")
}

// LastReadiness is the previous check's result (nil = never run).
func (a *App) LastReadiness(site string) *Readiness {
	s := a.State.FindSite(site)
	if s == nil {
		return nil
	}
	var r Readiness
	data, err := os.ReadFile(readinessPath(a, s.Name))
	if err != nil || json.Unmarshal(data, &r) != nil {
		return nil
	}
	ign := []string{}
	if s.Perf != nil {
		ign = s.Perf.Ignored
	}
	perf.MarkIgnored(r.Findings, ign)
	perf.SortFindings(r.Findings)
	return &r
}

// cliEnv is a site's command environment without the watcher's
// prepend (Mullion's own artisan calls aren't the user's commands).
func (a *App) cliEnv(s config.Site, extra map[string]string) []string {
	env := a.SiteEnv(s)
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "MULLION_WATCH") || strings.EqualFold(k, "PHP_INI_SCAN_DIR") && s.WatchPrepend() {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = setEnv(out, k, v)
	}
	return out
}

// captureCommand runs a command in the project and returns its stdout
// (stderr goes into the error when it fails).
func (a *App) captureCommand(ctx context.Context, s config.Site, command string, timeout time.Duration) ([]byte, error) {
	cmd := workers.ShellCommand(command)
	cmd.Dir = s.Path
	cmd.Env = a.cliEnv(s, nil)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(out.String())
			}
			if len(msg) > 1500 {
				msg = msg[:1500] + "…"
			}
			return out.Bytes(), fmt.Errorf("%s: %s", err, msg)
		}
		return out.Bytes(), nil
	case <-t.C:
		workers.KillTree(cmd.Process.Pid)
		<-done
		return nil, fmt.Errorf("`%s` took longer than %s", command, timeout)
	case <-ctx.Done():
		workers.KillTree(cmd.Process.Pid)
		<-done
		return nil, ctx.Err()
	}
}

// laravelFacts gathers what the PHP/Laravel rules read.
func (a *App) laravelFacts(ctx context.Context, s config.Site, info detect.Info) perf.Facts {
	f := perf.Facts{Laravel: info.Icon == "laravel", Now: time.Now()}
	f.LaravelVersion, _ = strconv.Atoi(strings.TrimLeft(info.FrameworkVersion, "^~v"))
	read := func(rel string) string {
		data, _ := os.ReadFile(filepath.Join(s.Path, rel))
		return string(data)
	}
	f.Env = redis.ParseEnv(read(".env"))
	for _, name := range []string{".env.production", ".env.prod"} {
		if text := read(name); text != "" {
			f.ProdEnv, f.ProdEnvFile = redis.ParseEnv(text), name
			break
		}
	}
	f.Composer = []byte(read("composer.json"))
	if f.Laravel {
		f.AppProvider = read(filepath.Join("app", "Providers", "AppServiceProvider.php"))
		f.TelescopeProvider = read(filepath.Join("app", "Providers", "TelescopeServiceProvider.php"))
		out, err := a.captureCommand(ctx, s, "php artisan route:list --json", 60*time.Second)
		if err == nil {
			f.Routes, err = perf.ParseRouteList(out)
		}
		if err != nil {
			f.RoutesErr = err.Error()
		}
		f.EnvCalls = perf.FindEnvCalls(phpSources(s.Path, []string{"app", "routes", filepath.Join("resources", "views"), "database", "bootstrap"}))
	}
	if v := a.SiteVersion(s); v != "" {
		f.PHPVersion = v
		if o, err := phpver.ReadOpcache(a.Paths, v); err == nil {
			f.Opcache = &perf.OpcacheFacts{Enabled: o.Enabled, Memory: o.Memory, MaxFiles: o.MaxFiles, ValidateTimestamps: o.ValidateTimestamps}
		}
	}
	f.PHPFiles = countPHPFiles(s.Path)
	return f
}

// phpSources reads the *.php files under dirs (relative paths → text),
// bounded. bootstrap/cache is skipped (generated).
func phpSources(root string, dirs []string) map[string]string {
	out := map[string]string{}
	total := 0
	for _, d := range dirs {
		_ = filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() {
				if e.Name() == "cache" || e.Name() == "vendor" || e.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".php") || total > 4000 {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil || len(data) > 1<<20 {
				return nil
			}
			total++
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(data)
			return nil
		})
	}
	return out
}

// countPHPFiles counts *.php under app/ and vendor/ (what OPcache has
// to hold), up to 500k.
func countPHPFiles(root string) int {
	n := 0
	for _, d := range []string{"app", "vendor", "src", "routes", "config"} {
		_ = filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil || n > 500000 {
				return filepath.SkipAll
			}
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".php") {
				n++
			}
			return nil
		})
	}
	return n
}

// schemaRunner runs the explain script for the schema read (tests swap it).
var schemaRunner = func(a *App, phpExe, ini string) (explain.Runner, error) {
	return explainRunner(a, phpExe, ini)
}

// readSchema reads the project's database schema through its .env.
func (a *App) readSchema(ctx context.Context, s config.Site) (*explain.Schema, error) {
	data, err := os.ReadFile(filepath.Join(s.Path, ".env"))
	if err != nil {
		return nil, errors.New("the project has no .env, so Mullion can't tell which database it uses")
	}
	c, _, err := explain.ConnFromEnv(redis.ParseEnv(string(data)), s.Path, "")
	if err != nil {
		return nil, err
	}
	v := a.SiteVersion(s)
	if v == "" {
		return nil, fmt.Errorf("no PHP version resolved for %s", s.Name)
	}
	dir := a.Paths.PhpVersionDir(v)
	run, err := schemaRunner(a, filepath.Join(dir, phpver.PhpExeName), filepath.Join(dir, "php.ini"))
	if err != nil {
		return nil, err
	}
	select {
	case explainSlots <- struct{}{}:
		defer func() { <-explainSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return explain.ReadSchema(ctx, run, c)
}

// buildCommand is the project's production build command ("" = none).
func buildCommand(dir string, info detect.Info) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil || !regexp.MustCompile(`"build"\s*:`).Match(data) {
		return ""
	}
	pm := info.PackageManager
	if pm == "" {
		pm = "npm"
	}
	return pm + " run build"
}

// CheckReadiness runs the production-readiness audit and keeps the
// result for the next visit.
func (a *App) CheckReadiness(ctx context.Context, site string) (*Readiness, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	info := detect.Project(s.Path)
	r := &Readiness{Site: s.Name, At: start.UTC().Truncate(time.Second), Findings: []perf.Finding{}, Notes: []string{}}
	php := s.IsPHP() && (fileExists(filepath.Join(s.Path, "composer.json")) || info.Language == "PHP")
	if php {
		r.Findings = append(r.Findings, perf.CheckLaravel(a.laravelFacts(ctx, *s, info))...)
		if info.Icon == "laravel" {
			if sc, err := a.readSchema(ctx, *s); err != nil {
				r.Notes = append(r.Notes, "Database checks skipped: "+err.Error())
			} else {
				r.Schema = &SchemaSummary{Engine: sc.Engine, Conn: sc.Conn, Tables: len(sc.Tables), FKs: len(sc.ForeignKeys)}
				r.Findings = append(r.Findings, perf.CheckSchema(sc)...)
				if sc.Truncated {
					r.Notes = append(r.Notes, "The database has more than 400 tables; only the first 400 were checked.")
				}
			}
		}
		st := watch.SettingsOf(*s)
		if st.Enabled && st.Deep {
			items := watch.NewFeed(a.Paths, *s, a.State.Host(*s)).Recent(2000)
			r.Findings = append(r.Findings, perf.CheckQueries(perf.SummarizeQueries(items, float64(st.SlowQueryMs), 8))...)
		} else {
			r.Notes = append(r.Notes, "Turn on the Watch tab's Deep mode to include the N+1 and slow queries seen while you use the site.")
		}
	}
	if fileExists(filepath.Join(s.Path, "package.json")) {
		dir := perf.FindBuildDir(s.Path)
		if s.Kind == "node" && s.BuildDir != "" {
			dir = s.BuildDir
		}
		br := perf.AnalyzeBuild(s.Path, dir)
		r.Build = &br
		r.Findings = append(r.Findings, perf.CheckFrontend(br, buildCommand(s.Path, info))...)
	}
	r.Ms = time.Since(start).Milliseconds()
	_ = writePerfJSON(readinessPath(a, s.Name), r)
	ign := []string{}
	if s.Perf != nil {
		ign = s.Perf.Ignored
	}
	perf.MarkIgnored(r.Findings, ign)
	perf.SortFindings(r.Findings)
	return r, nil
}

func writePerfJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFile(path, string(data))
}

// ── production-like mode ─────────────────────────────────────────────

// prodEnv is what production-like mode passes PHP as FastCGI params.
// APP_ENV is left out when the project has a .env.<that environment>
// file: Laravel (and Symfony) would then load it — production's own
// settings, maybe its database — instead of .env.
func prodEnv(s config.Site) map[string]string {
	env := map[string]string{"APP_ENV": "production", "APP_DEBUG": "false"}
	if detect.Project(s.Path).Icon == "symfony" {
		env = map[string]string{"APP_ENV": "prod", "APP_DEBUG": "0"}
	}
	if prodEnvFileClash(s, env) != "" {
		delete(env, "APP_ENV")
	}
	return env
}

// prodEnvFileClash is the .env.<APP_ENV> file that keeps APP_ENV out
// of production-like mode ("" = none).
func prodEnvFileClash(s config.Site, env map[string]string) string {
	if v := env["APP_ENV"]; v != "" {
		name := ".env." + v
		if fileExists(filepath.Join(s.Path, name)) {
			return name
		}
	}
	return ""
}

// prodCaddyConf is the site's Caddyfile production-like block (nil = off).
func (a *App) prodCaddyConf(s config.Site) *caddy.ProdConf {
	if !s.ProdMode() {
		return nil
	}
	if s.IsPHP() {
		return &caddy.ProdConf{Env: prodEnv(s)}
	}
	return &caddy.ProdConf{}
}

// prodFreezesOpcache: php-fpm takes the per-site PHP_VALUE; Windows'
// php-cgi doesn't (one php-cgi serves every site of a version), so
// there production-like mode leaves OPcache as configured.
var prodFreezesOpcache = runtime.GOOS != "windows"

// pinValidateTimestamps sets the OPcache timestamp check for PHP sites:
// "0" for production-like ones and, while any site of a PHP version is
// production-like, its configured value for the others on that version.
func (a *App) pinValidateTimestamps(confs []caddy.SiteConf) {
	if !prodFreezesOpcache {
		return
	}
	frozen := map[int]bool{}
	for i := range confs {
		if confs[i].Prod != nil && confs[i].FcgiPort > 0 && (confs[i].Kind == "" || confs[i].Kind == "php") {
			confs[i].ValidateTimestamps = "0"
			frozen[confs[i].FcgiPort] = true
		}
	}
	if len(frozen) == 0 {
		return
	}
	cfgVal := map[int]string{}
	for i := range confs {
		c := &confs[i]
		if c.Prod != nil || !frozen[c.FcgiPort] || c.Kind != "" && c.Kind != "php" {
			continue
		}
		v, ok := cfgVal[c.FcgiPort]
		if !ok {
			v = "1"
			if s := a.State.FindSite(c.Name); s != nil {
				if o, err := phpver.ReadOpcache(a.Paths, a.SiteVersion(*s)); err == nil && !o.ValidateTimestamps {
					v = "0"
				}
			}
			cfgVal[c.FcgiPort] = v
		}
		c.ValidateTimestamps = v
	}
}

// ProdStatus describes a site's production-like mode.
type ProdStatus struct {
	On        bool              `json:"on"`
	Since     int64             `json:"since,omitempty"`
	Kind      string            `json:"kind"`
	Laravel   bool              `json:"laravel"`
	Optimized bool              `json:"optimized"`
	Env       map[string]string `json:"env,omitempty"`
	// OpcacheFrozen: OPcache stops checking timestamps for this site.
	OpcacheFrozen bool     `json:"opcacheFrozen"`
	Notes         []string `json:"notes"`
}

// ProdModeStatus reports a site's production-like mode.
func (a *App) ProdModeStatus(site string) (ProdStatus, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return ProdStatus{}, err
	}
	st := ProdStatus{On: s.ProdMode(), Kind: siteKindOf(*s), Laravel: detect.Project(s.Path).Icon == "laravel", Notes: []string{}}
	if s.Perf != nil {
		st.Since, st.Optimized = s.Perf.ProdSince, s.Perf.ProdOptimized
	}
	if s.IsPHP() {
		st.Env = prodEnv(*s)
		want := map[string]string{"APP_ENV": "production"}
		if detect.Project(s.Path).Icon == "symfony" {
			want["APP_ENV"] = "prod"
		}
		if f := prodEnvFileClash(*s, want); f != "" {
			st.Notes = append(st.Notes, "The project has "+f+", which the framework would load instead of .env if APP_ENV were "+want["APP_ENV"]+" (production's settings, maybe its database) — so production-like mode keeps APP_ENV from .env and only turns debug off.")
		}
		st.OpcacheFrozen = prodFreezesOpcache
		if !prodFreezesOpcache {
			st.Notes = append(st.Notes, "On Windows one php-cgi serves every site of a PHP version and ignores per-site PHP settings, so OPcache keeps checking file timestamps here; APP_ENV/APP_DEBUG and the caches still apply.")
		}
	}
	return st, nil
}

func siteKindOf(s config.Site) string {
	if s.Kind == "" {
		return "php"
	}
	return s.Kind
}

// prodCommand builds a command with the production-like env to a command's environment
// (for `artisan optimize`: the config cache must see APP_ENV=production).
func (a *App) prodCommand(s config.Site, command string) *exec.Cmd {
	cmd := workers.ShellCommand(command)
	cmd.Dir = s.Path
	cmd.Env = a.cliEnv(s, prodEnv(s))
	return cmd
}

// SetProdMode turns a site's production-like mode on or off. optimize
// (Laravel) also caches config, routes, views and events with
// `php artisan optimize` — run with the production env so the cached
// config has APP_DEBUG=false — and turning the mode off runs
// `optimize:clear`. onOutput streams the commands' output.
func (a *App) SetProdMode(ctx context.Context, site string, on, optimize bool, onOutput func([]byte)) error {
	s, err := a.siteOrErr(site)
	if err != nil {
		return err
	}
	say := func(msg string) {
		if onOutput != nil {
			onOutput([]byte(msg + "\n"))
		}
	}
	p := ensurePerf(s)
	laravel := detect.Project(s.Path).Icon == "laravel"
	if on == p.Prod {
		if !on || !optimize || p.ProdOptimized || !laravel {
			return nil
		}
	}
	switch {
	case s.Kind == "node":
		if on {
			if s.Mode != "build" {
				say("Building the production bundle (if there is none yet)…")
				dir, err := a.EnsureBuildOutput(s.Path, s.BuildDir)
				if err != nil {
					return err
				}
				p.ProdPrevMode = s.Mode
				if p.ProdPrevMode == "" {
					p.ProdPrevMode = "dev"
				}
				s.BuildDir = dir
				s.Mode = "build"
				devserver.Stop(a.Paths, s.Name)
			} else {
				p.ProdPrevMode = "build"
			}
		} else {
			if p.ProdPrevMode != "" && p.ProdPrevMode != "build" {
				s.Mode = p.ProdPrevMode
				s.DevPaused = false
			}
			p.ProdPrevMode = ""
		}
	case s.IsPHP():
		if on && optimize && laravel {
			var kv []string
			for k, v := range prodEnv(*s) {
				kv = append(kv, k+"="+v)
			}
			sort.Strings(kv)
			say("$ php artisan optimize   (with " + strings.Join(kv, " ") + ")")
			code, err := runProjectCmd(ctx, a.prodCommand(*s, "php artisan optimize"), "php artisan optimize", onOutput)
			if err != nil {
				return err
			}
			if code != 0 {
				// Leave nothing half-cached behind.
				_, _ = runProjectCmd(ctx, a.prodCommand(*s, "php artisan optimize:clear"), "php artisan optimize:clear", onOutput)
				return fmt.Errorf("`php artisan optimize` failed (exit %d) — production-like mode stays off", code)
			}
			p.ProdOptimized = true
		}
		if !on && p.ProdOptimized {
			say("$ php artisan optimize:clear")
			cmd := workers.ShellCommand("php artisan optimize:clear")
			cmd.Dir = s.Path
			cmd.Env = a.cliEnv(*s, nil)
			if code, err := runProjectCmd(ctx, cmd, "php artisan optimize:clear", onOutput); err != nil || code != 0 {
				say("warning: optimize:clear did not finish cleanly — run it from the Commands tab")
			}
			p.ProdOptimized = false
		}
	}
	p.Prod = on
	if on {
		p.ProdSince = time.Now().Unix()
	} else {
		p.ProdSince = 0
	}
	say("Updating the site's Caddy and PHP settings…")
	if err := a.applyIfEnabled(); err != nil {
		return err
	}
	// php-fpm keeps a PHP_VALUE for the later requests its worker
	// serves: a fresh worker is the only way to drop the frozen
	// timestamps check (and the cached opcodes) everywhere.
	if s.IsPHP() && prodFreezesOpcache && !a.skipApply {
		if v := a.SiteVersion(*s); v != "" {
			say("Restarting PHP " + v + " so every site sees fresh OPcache settings…")
			if err := fcgi.StopVersion(a.Paths, v); err == nil {
				_ = fcgi.Ensure(a.Paths, v)
			}
		}
	}
	return nil
}

// ── load tests ───────────────────────────────────────────────────────

// LoadInput is a load test request from the panel or the CLI.
type LoadInput struct {
	URL         string            `json:"url"` // absolute, or a path on the site's host
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	Concurrency int               `json:"concurrency"`
	DurationS   int               `json:"durationS"`
	Requests    int               `json:"requests"`
	WarmUpS     int               `json:"warmUpS"`
}

// LoadMaxRequests caps one test.
const LoadMaxRequests = 100000

// LoadConfig validates a load test against the site: the target must be
// one of the site's own hosts (its domain, its aliases) or localhost on
// its dev server's port — never anything else.
func (a *App) LoadConfig(site string, in LoadInput) (loadtest.Config, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return loadtest.Config{}, err
	}
	host := a.State.Host(*s)
	raw := strings.TrimSpace(in.URL)
	scheme := "http"
	if s.Secure {
		scheme = "https"
	}
	if raw == "" {
		raw = "/"
	}
	if strings.HasPrefix(raw, "/") {
		raw = scheme + "://" + host + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return loadtest.Config{}, fmt.Errorf("%q isn't a URL on %s", in.URL, host)
	}
	h := strings.ToLower(u.Hostname())
	allowed := false
	for _, sh := range SiteHosts(*s, a.State.Config.TLD) {
		sh = strings.ToLower(sh)
		if h == sh || strings.HasPrefix(sh, "*.") && strings.HasSuffix(h, sh[1:]) {
			allowed = true
		}
	}
	if !allowed && (h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()) {
		port, _ := strconv.Atoi(u.Port())
		allowed = s.DevPort > 0 && port == s.DevPort
	}
	if !allowed {
		return loadtest.Config{}, fmt.Errorf("load tests only run against %s's own hosts (%s) — not %s", s.Name, strings.Join(SiteHosts(*s, a.State.Config.TLD), ", "), u.Host)
	}
	c := loadtest.Config{
		URL: u.String(), Method: strings.ToUpper(strings.TrimSpace(in.Method)), Body: in.Body,
		Concurrency: in.Concurrency, Duration: time.Duration(in.DurationS) * time.Second, Requests: in.Requests,
		WarmUp: time.Duration(in.WarmUpS) * time.Second, MaxRequests: LoadMaxRequests, Timeout: 30 * time.Second,
		Headers: map[string]string{},
	}
	if c.Method == "" {
		c.Method = "GET"
	}
	if c.Duration > 0 {
		c.Requests = 0
	}
	for k, v := range in.Headers {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, "\r\n:") || strings.ContainsAny(v, "\r\n") {
			return c, fmt.Errorf("header %q isn't valid", k)
		}
		c.Headers[k] = v
	}
	if _, ok := c.Headers["User-Agent"]; !ok {
		c.Headers["User-Agent"] = "Mullion-loadtest"
	}
	if u.Scheme == "https" {
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if pem, err := os.ReadFile(caddy.RootCertPath()); err == nil && pool.AppendCertsFromPEM(pem) {
			c.RootCAs = pool
		} else if strings.HasSuffix(h, "."+a.State.Config.TLD) {
			// No local CA on disk yet: these hosts are ours, by name.
			c.InsecureSkipVerify = true
		}
	}
	// Keep a sleeping dev server awake: the idle watcher looks at the
	// access log's time.
	accessLog := filepath.Join(a.Paths.LogsDir(), host+".log")
	c.Activity = func() {
		now := time.Now()
		_ = os.Chtimes(accessLog, now, now)
	}
	return c, loadtest.Validate(c)
}

// SaveLoad stores a finished test.
func (a *App) SaveLoad(site string, in LoadInput, c loadtest.Config, snap loadtest.Snapshot) (perf.LoadResult, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return perf.LoadResult{}, err
	}
	mode := "dev"
	if s.ProdMode() {
		mode = "prod"
	}
	r := perf.LoadResult{URL: c.URL, Method: c.Method, Concurrency: c.Concurrency, DurationS: int(c.Duration / time.Second),
		Requests: c.Requests, WarmUpS: int(c.WarmUp / time.Second), Mode: mode, Result: snap}
	return perf.SaveLoadResult(a.Paths, s.Name, r)
}

// ── workers ──────────────────────────────────────────────────────────

// WorkerTargets lists every site's running workers (for the sampler).
func (a *App) WorkerTargets() []workers.Target {
	var out []workers.Target
	for _, s := range a.State.Sites {
		for _, w := range s.Workers {
			st := workers.Status(a.Paths, s, w)
			if st.Running && st.Pid > 0 {
				out = append(out, workers.Target{Site: s.Name, ID: w.ID, Pid: st.Pid})
			}
		}
	}
	return out
}

// WorkerHealth is one worker's health as the panel shows it.
type WorkerHealth struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Kind     string               `json:"kind"`
	Command  string               `json:"command"`
	Status   workers.WorkerStatus `json:"status"`
	Samples  []workers.Sample     `json:"samples"`
	Restarts []int64              `json:"restartTimes"`
	Leak     workers.Leak         `json:"leak"`
	Queue    *workers.QueueStats  `json:"queue,omitempty"`
}

// HealthSource supplies sampled history (the panel's in-memory sampler;
// nil = what is persisted on disk).
type HealthSource interface {
	History(site, id string) []workers.Sample
	Restarts(site, id string) []int64
}

// WorkersHealth describes a site's workers: memory and CPU history, a
// leak verdict, and for queue workers the jobs their log shows.
func (a *App) WorkersHealth(site string, src HealthSource) ([]WorkerHealth, error) {
	s, err := a.siteOrErr(site)
	if err != nil {
		return nil, err
	}
	out := []WorkerHealth{}
	for _, w := range s.Workers {
		h := WorkerHealth{ID: w.ID, Name: w.Name, Kind: w.Kind, Command: w.Command, Status: workers.Status(a.Paths, *s, w), Restarts: []int64{}}
		if src != nil {
			h.Samples = src.History(s.Name, w.ID)
			h.Restarts = src.Restarts(s.Name, w.ID)
		}
		if len(h.Samples) == 0 {
			h.Samples = workers.LoadHistory(a.Paths, s.Name, w.ID)
		}
		if h.Samples == nil {
			h.Samples = []workers.Sample{}
		}
		h.Leak = workers.DetectLeak(h.Samples)
		if w.Kind == "queue" || strings.Contains(w.Command, "queue:work") || strings.Contains(w.Command, "queue:listen") || strings.Contains(w.Command, "horizon") {
			q := workers.QueueStatsOf(a.Paths, s.Name, w.ID, 3000)
			h.Queue = &q
		}
		out = append(out, h)
	}
	return out, nil
}
