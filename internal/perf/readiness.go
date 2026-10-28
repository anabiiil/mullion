package perf

// The production-readiness audit: rules over facts Mullion gathers from
// the project (its .env files, composer.json, `artisan route:list`,
// source files), the site's PHP and OPcache settings, the database
// schema (schema.go), the frontend build (frontend.go) and what the
// Watch tab saw. Every finding has a stable id (so the user can ignore
// it), a plain explanation, and the exact fix.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Severity of a finding, most urgent first.
const (
	SevHigh   = "high"
	SevMedium = "medium"
	SevLow    = "low"
	SevInfo   = "info"
)

var sevRank = map[string]int{SevHigh: 0, SevMedium: 1, SevLow: 2, SevInfo: 3}

// Finding is one readiness result.
type Finding struct {
	ID       string `json:"id"`
	Category string `json:"category"` // laravel, php, database, frontend, queries
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Why      string `json:"why"`
	// Fix is what to change: a snippet, ini lines, a command.
	Fix string `json:"fix,omitempty"`
	// Command can be run from the Commands tab ("Run fix").
	Command string `json:"command,omitempty"`
	// Caution is shown before Command runs (e.g. caching config in
	// development freezes .env changes).
	Caution string `json:"caution,omitempty"`
	// Where lists file:line places (env() calls, closures…).
	Where []string `json:"where,omitempty"`
	// Explain is a recorded query the panel can EXPLAIN.
	Explain *ExplainRef `json:"explain,omitempty"`
	Ignored bool        `json:"ignored,omitempty"`
}

// ExplainRef is a query seen by the Watch tab.
type ExplainRef struct {
	SQL      string `json:"sql"`
	Bindings []any  `json:"bindings"`
	Conn     string `json:"conn,omitempty"`
	NPlusOne int    `json:"nPlusOne,omitempty"`
}

// RouteInfo is one route from `php artisan route:list --json`.
type RouteInfo struct {
	Method string `json:"method"`
	URI    string `json:"uri"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

// ParseRouteList reads `route:list --json` output (anything before the
// JSON array — a deprecation notice — is skipped).
func ParseRouteList(out []byte) ([]RouteInfo, error) {
	s := string(out)
	i := strings.Index(s, "[")
	if i < 0 {
		return nil, fmt.Errorf("route:list printed no JSON")
	}
	var rs []RouteInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(s[i:])), &rs); err != nil {
		return nil, fmt.Errorf("route:list printed unexpected output: %v", err)
	}
	return rs, nil
}

// SourceHit is a matching line in the project's code.
type SourceHit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// OpcacheFacts is the site's PHP OPcache configuration.
type OpcacheFacts struct {
	Enabled            bool
	Memory             int // MB
	MaxFiles           int
	ValidateTimestamps bool
}

// Facts is everything the PHP/Laravel rules look at.
type Facts struct {
	Laravel        bool
	LaravelVersion int // major (0 = unknown)
	// Env is the project's .env; ProdEnv the production-like file next
	// to it (.env.production, else .env.example) and ProdEnvFile its name.
	Env         map[string]string
	ProdEnv     map[string]string
	ProdEnvFile string
	Composer    []byte // composer.json
	// Routes from route:list (RoutesErr when it failed — the app can't
	// boot from the CLI, which every cache command needs).
	Routes    []RouteInfo
	RoutesErr string
	// AppProvider is app/Providers/AppServiceProvider.php.
	AppProvider string
	// EnvCalls are env() calls outside config/.
	EnvCalls []SourceHit
	// TelescopeGated: app/Providers/TelescopeServiceProvider.php has a
	// gate() with someone in it.
	TelescopeProvider string
	PHPVersion        string // "8.4.23"
	Opcache           *OpcacheFacts
	// PHPFiles counts *.php under app/ and vendor/ (what OPcache holds).
	PHPFiles int
	Now      time.Time
}

// devPackages are Composer packages that belong in require-dev.
var devPackages = map[string]string{
	"barryvdh/laravel-debugbar":         "Debugbar renders a toolbar with queries and request data — in production it leaks internals and slows every page.",
	"barryvdh/laravel-ide-helper":       "IDE helper only generates stubs for your editor.",
	"laravel/sail":                      "Sail is the Docker dev environment.",
	"laravel/pint":                      "Pint is a code formatter.",
	"laravel/dusk":                      "Dusk drives a browser for tests — its routes must never exist in production.",
	"fakerphp/faker":                    "Faker generates fake data for factories and seeders.",
	"mockery/mockery":                   "Mockery is a test library.",
	"phpunit/phpunit":                   "PHPUnit is the test runner.",
	"pestphp/pest":                      "Pest is the test runner.",
	"nunomaduro/collision":              "Collision formats CLI errors for development.",
	"spatie/laravel-ignition":           "Ignition is the development error page (it had a remote code execution bug when exposed).",
	"filp/whoops":                       "Whoops is a development error page.",
	"beyondcode/laravel-query-detector": "The query detector is a development tool.",
	"laravel/pail":                      "Pail tails logs in your terminal.",
	"itsgoingd/clockwork":               "Clockwork exposes request profiles over HTTP.",
}

type composerJSON struct {
	Require    map[string]string `json:"require"`
	RequireDev map[string]string `json:"require-dev"`
	Config     map[string]any    `json:"config"`
	Extra      struct {
		Laravel struct {
			DontDiscover []string `json:"dont-discover"`
		} `json:"laravel"`
	} `json:"extra"`
}

// phpEOL is when each PHP branch stops getting security fixes
// (php.net/supported-versions).
var phpEOL = map[string]string{
	"7.4": "2022-11-28", "8.0": "2023-11-26", "8.1": "2025-12-31", "8.2": "2026-12-31",
	"8.3": "2027-12-31", "8.4": "2028-12-31", "8.5": "2029-12-31",
}

// CheckLaravel runs the PHP, Composer and Laravel rules.
func CheckLaravel(f Facts) []Finding {
	if f.Now.IsZero() {
		f.Now = time.Now()
	}
	var out []Finding
	add := func(fd Finding) { out = append(out, fd) }

	if f.Laravel {
		out = append(out, envRules(f)...)
		out = append(out, cacheRules(f)...)
		if !regexp.MustCompile(`preventLazyLoading|shouldBeStrict`).MatchString(f.AppProvider) {
			add(Finding{ID: "laravel.strict-models", Category: "laravel", Severity: SevLow,
				Title: "Lazy loading isn't prevented in development",
				Why:   "N+1 queries are the most common Laravel slowdown. Model::preventLazyLoading() turns every lazy-loaded relation into an exception while you develop, so you add with() before it ships. shouldBeStrict() also catches silently discarded attributes and missing attributes.",
				Fix:   "// app/Providers/AppServiceProvider.php — boot()\nuse Illuminate\\Database\\Eloquent\\Model;\n\nModel::shouldBeStrict(! $this->app->isProduction());"})
		}
	}
	out = append(out, composerRules(f)...)
	out = append(out, phpRules(f)...)
	return out
}

// envValue picks what to judge a key by: the production-like file when
// it sets it (fromProd), else the local .env.
func (f Facts) envValue(key string) (val string, fromProd, set bool) {
	if v, ok := f.ProdEnv[key]; ok {
		return v, true, true
	}
	v, ok := f.Env[key]
	return v, false, ok
}

func envRules(f Facts) []Finding {
	var out []Finding
	prodName := f.ProdEnvFile
	where := func(fromProd bool) string {
		if fromProd {
			return prodName
		}
		return ".env"
	}
	// sev: the production-like file is wrong = real; only the local .env
	// says it (fine for development) = a reminder.
	sev := func(fromProd bool, real string) string {
		if fromProd {
			return real
		}
		return SevInfo
	}
	note := func(fromProd bool) string {
		if fromProd {
			return ""
		}
		if prodName == "" {
			return " (Mullion can't see your production server — this is your local .env, which is fine for development; make sure production's .env differs.)"
		}
		return " (" + prodName + " doesn't set it; this is your local .env.)"
	}
	if v, fp, _ := f.envValue("APP_DEBUG"); isTrue(v) {
		out = append(out, Finding{ID: "laravel.app-debug", Category: "laravel", Severity: sev(fp, SevHigh),
			Title: "APP_DEBUG is true in " + where(fp),
			Why:   "With debug on, every error page shows the stack trace, environment variables and database credentials to whoever triggers it." + note(fp),
			Fix:   "APP_DEBUG=false"})
	}
	if v, fp, _ := f.envValue("APP_ENV"); strings.ToLower(strings.TrimSpace(v)) != "production" {
		shown := v
		if shown == "" {
			shown = "unset"
		}
		out = append(out, Finding{ID: "laravel.app-env", Category: "laravel", Severity: sev(fp, SevMedium),
			Title: "APP_ENV is " + shown + " in " + where(fp),
			Why:   "Laravel and packages switch production behaviour on APP_ENV=production: artisan asks before destructive commands, Telescope and debug tools stay off, isProduction() checks work." + note(fp),
			Fix:   "APP_ENV=production"})
	}
	if v, fp, _ := f.envValue("LOG_LEVEL"); func() bool {
		l := strings.ToLower(strings.TrimSpace(v))
		return l == "" || l == "debug" || l == "info"
	}() {
		shown := v
		if shown == "" {
			shown = "unset (debug)"
		}
		out = append(out, Finding{ID: "laravel.log-level", Category: "laravel", Severity: sev(fp, SevLow),
			Title: "LOG_LEVEL is " + shown + " in " + where(fp),
			Why:   "Debug logging writes every Log::debug() call (and packages' chatter) to disk on every request — slower, and the logs fill up." + note(fp),
			Fix:   "LOG_LEVEL=warning"})
	}
	if v, _, _ := f.envValue("APP_KEY"); strings.TrimSpace(f.Env["APP_KEY"]) == "" && strings.TrimSpace(v) == "" {
		out = append(out, Finding{ID: "laravel.app-key", Category: "laravel", Severity: SevHigh,
			Title: "APP_KEY is empty",
			Why:   "Laravel encrypts cookies and sessions with APP_KEY; without it every request fails with a MissingAppKeyException. Production needs its own key, generated once and kept.",
			Fix:   "php artisan key:generate", Command: "php artisan key:generate",
			Caution: "This writes a new APP_KEY into .env."})
	}
	if v, fp, _ := f.envValue("QUEUE_CONNECTION"); strings.EqualFold(strings.TrimSpace(v), "sync") {
		out = append(out, Finding{ID: "laravel.queue-sync", Category: "laravel", Severity: sev(fp, SevMedium),
			Title: "QUEUE_CONNECTION=sync in " + where(fp),
			Why:   "The sync driver runs every queued job inside the request that dispatched it — mail, exports and API calls make the user wait. Use database or redis with a queue worker." + note(fp),
			Fix:   "QUEUE_CONNECTION=redis   # or database, with `php artisan queue:work` supervised"})
	}
	for _, k := range []string{"SESSION_DRIVER", "CACHE_STORE", "CACHE_DRIVER"} {
		v, fp, set := f.envValue(k)
		if !set {
			continue
		}
		d := strings.ToLower(strings.TrimSpace(v))
		if d != "file" && d != "database" && d != "array" {
			continue
		}
		sv := SevLow
		why := "The " + d + " driver works on one server, but it's slower than Redis or Memcached under load"
		switch d {
		case "array":
			sv = SevHigh
			why = "The array driver forgets everything at the end of each request"
		case "database":
			why = "The database driver adds a query (and a write) to every request, competing with your own queries"
		case "file":
			why = "The file driver doesn't work across several servers and gets slow with many keys"
		}
		if !fp {
			sv = SevInfo
		}
		id := "laravel." + strings.ToLower(strings.ReplaceAll(k, "_", "-"))
		out = append(out, Finding{ID: id, Category: "laravel", Severity: sv,
			Title: k + "=" + d + " in " + where(fp),
			Why:   why + "." + note(fp),
			Fix:   k + "=redis"})
	}
	return out
}

func cacheRules(f Facts) []Finding {
	var out []Finding
	if f.RoutesErr != "" {
		out = append(out, Finding{ID: "laravel.boot", Category: "laravel", Severity: SevHigh,
			Title: "The app doesn't boot from the command line",
			Why:   "`php artisan route:list` failed, so `config:cache`, `route:cache` and deployments would fail too: " + firstLine(f.RoutesErr),
			Fix:   "php artisan route:list", Command: "php artisan route:list"})
	} else if len(f.Routes) > 0 {
		var closures []string
		names := map[string][]string{}
		for _, r := range f.Routes {
			if strings.EqualFold(r.Action, "Closure") {
				closures = append(closures, r.Method+" /"+strings.TrimPrefix(r.URI, "/"))
			}
			if r.Name != "" {
				names[r.Name] = append(names[r.Name], r.Method+" /"+strings.TrimPrefix(r.URI, "/"))
			}
		}
		var dups []string
		for n, rs := range names {
			// The same route listed per method (GET|HEAD) is one route.
			if len(uniq(rs)) > 1 {
				dups = append(dups, n+": "+strings.Join(uniq(rs), ", "))
			}
		}
		sort.Strings(dups)
		if len(dups) > 0 {
			out = append(out, Finding{ID: "laravel.route-names", Category: "laravel", Severity: SevHigh,
				Title: "Two routes share a name — route:cache will fail",
				Why:   "`php artisan route:cache` refuses to cache routes when a name is assigned twice (\"Another route has already been assigned name\"), so a deploy that caches routes breaks.",
				Where: dups, Fix: "Give each route its own ->name(...)."})
		}
		if len(closures) > 0 {
			sev, why := SevLow, "Laravel 9+ serializes closure routes into the route cache, so caching works — but controllers keep routes/web.php readable and testable."
			if f.LaravelVersion > 0 && f.LaravelVersion < 9 {
				sev, why = SevHigh, "Before Laravel 9, `php artisan route:cache` fails on closure routes (\"Unable to prepare route for serialization. Uses Closure.\")."
			}
			out = append(out, Finding{ID: "laravel.route-closures", Category: "laravel", Severity: sev,
				Title: strconv.Itoa(len(closures)) + " route(s) use a closure",
				Why:   why, Where: capList(closures, 20),
				Fix: "Route::get('/x', [XController::class, 'show']);"})
		}
	}
	if len(f.EnvCalls) > 0 {
		var where []string
		for _, h := range f.EnvCalls {
			where = append(where, h.File+":"+strconv.Itoa(h.Line)+"  "+h.Text)
		}
		out = append(out, Finding{ID: "laravel.env-outside-config", Category: "laravel", Severity: SevMedium,
			Title: "env() is called outside config/",
			Why:   "Once `php artisan config:cache` runs (every production deploy should), the .env isn't loaded and env() returns null everywhere except inside config files. Read the value through config() instead.",
			Where: capList(where, 30),
			Fix:   "// config/services.php\n'thing' => ['key' => env('THING_KEY')],\n\n// in the code\nconfig('services.thing.key')"})
	}
	if f.RoutesErr == "" {
		out = append(out, Finding{ID: "laravel.optimize", Category: "laravel", Severity: SevInfo,
			Title: "Cache config, routes, views and events when you deploy",
			Why:   "`php artisan optimize` caches the configuration, routes, compiled views and the event map — each request then skips reading and merging dozens of files. Run it as part of every deploy (and `optimize:clear` before changing .env).",
			Fix:   "php artisan optimize", Command: "php artisan optimize",
			Caution: "Running this here caches your local config: changes to .env and config/ stop showing until you run `php artisan optimize:clear`. Production-like mode can do this for you and undo it when you turn the mode off."})
	}
	return out
}

func composerRules(f Facts) []Finding {
	if len(f.Composer) == 0 {
		return nil
	}
	var c composerJSON
	if err := json.Unmarshal(f.Composer, &c); err != nil {
		return []Finding{{ID: "composer.json", Category: "php", Severity: SevMedium, Title: "composer.json can't be read", Why: err.Error()}}
	}
	var out []Finding
	var names []string
	for name := range c.Require {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if why, ok := devPackages[name]; ok {
			out = append(out, Finding{ID: "composer.dev-in-require:" + name, Category: "php", Severity: SevMedium,
				Title: name + " is in require, not require-dev",
				Why:   why + " In require it is installed (and, in Laravel, auto-discovered and booted) in production, where `composer install --no-dev` would otherwise leave it out.",
				Fix:   "composer remove " + name + " && composer require --dev " + name})
		}
	}
	if _, ok := c.Require["laravel/telescope"]; ok {
		v, fp, _ := f.envValue("TELESCOPE_ENABLED")
		gated := regexp.MustCompile(`(?s)function\s+gate\s*\(.*?in_array\(\s*\$user->email\s*,\s*\[\s*['"]`).MatchString(f.TelescopeProvider)
		if !(fp && !isTrue(v) && strings.TrimSpace(v) != "") {
			sev := SevMedium
			why := "Telescope is in require, so it is installed in production. It records every request, query and job into the database (slow, and the table grows fast)"
			if !gated {
				why += ", and its gate() lets no one in only while the email list is empty — check TelescopeServiceProvider::gate()"
			}
			out = append(out, Finding{ID: "composer.telescope", Category: "php", Severity: sev,
				Title: "Telescope would run in production",
				Why:   why + ". Move it to require-dev (and register it only locally), or set TELESCOPE_ENABLED=false in production.",
				Fix:   "composer remove laravel/telescope && composer require --dev laravel/telescope\n\n// composer.json\n\"extra\": {\"laravel\": {\"dont-discover\": [\"laravel/telescope\"]}}\n\n// AppServiceProvider::register()\nif ($this->app->environment('local')) {\n    $this->app->register(\\Laravel\\Telescope\\TelescopeServiceProvider::class);\n    $this->app->register(TelescopeServiceProvider::class);\n}"})
		}
	}
	opt, _ := c.Config["optimize-autoloader"].(bool)
	auth, _ := c.Config["classmap-authoritative"].(bool)
	if !opt && !auth {
		out = append(out, Finding{ID: "composer.optimize-autoloader", Category: "php", Severity: SevLow,
			Title: "Composer's autoloader isn't optimized",
			Why:   "Without an optimized classmap, every class load checks the PSR-4 folders on disk. Optimize it when you deploy (or set it in composer.json so every install does).",
			Fix:   "composer install --no-dev --optimize-autoloader\n\n// or composer.json\n\"config\": {\"optimize-autoloader\": true}"})
	}
	if !auth {
		out = append(out, Finding{ID: "composer.classmap-authoritative", Category: "php", Severity: SevInfo,
			Title: "classmap-authoritative is off",
			Why:   "With an authoritative classmap Composer never touches the filesystem for a class it doesn't know — a little faster still. Only safe when no classes are generated at runtime.",
			Fix:   "composer dump-autoload --classmap-authoritative --no-dev"})
	}
	return out
}

func phpRules(f Facts) []Finding {
	var out []Finding
	if mm := majorMinor(f.PHPVersion); mm != "" {
		if eol, ok := phpEOL[mm]; ok {
			end, _ := time.Parse("2006-01-02", eol)
			switch {
			case f.Now.After(end):
				out = append(out, Finding{ID: "php.eol", Category: "php", Severity: SevHigh,
					Title: "PHP " + mm + " no longer gets security fixes",
					Why:   "PHP " + mm + " reached its end of life on " + eol + ". Production should run a supported version (newer ones are also faster).",
					Fix:   "Upgrade the site's PHP (Settings → PHP) and production's to 8.4 or newer."})
			case end.Sub(f.Now) < 120*24*time.Hour:
				out = append(out, Finding{ID: "php.eol-soon", Category: "php", Severity: SevMedium,
					Title: "PHP " + mm + " stops getting security fixes on " + eol,
					Why:   "Plan the upgrade before then; newer PHP versions are also faster.",
					Fix:   "Upgrade to PHP 8.4 or newer."})
			}
		}
	}
	if o := f.Opcache; o != nil {
		if !o.Enabled {
			out = append(out, Finding{ID: "php.opcache-off", Category: "php", Severity: SevHigh,
				Title: "OPcache is off",
				Why:   "Without OPcache PHP compiles every file on every request — typically 2–5× slower. Production must run with it on.",
				Fix:   "opcache.enable=1"})
		}
		if f.PHPFiles > 0 && o.MaxFiles < f.PHPFiles {
			out = append(out, Finding{ID: "php.opcache-max-files", Category: "php", Severity: SevMedium,
				Title: "OPcache can hold " + strconv.Itoa(o.MaxFiles) + " files; the project has " + strconv.Itoa(f.PHPFiles),
				Why:   "Once opcache.max_accelerated_files is reached, the remaining files are compiled on every request (and the cache restarts when it fills). Set it above the project's file count (app/ + vendor/).",
				Fix:   "opcache.max_accelerated_files=" + strconv.Itoa(nextOpcacheSize(f.PHPFiles))})
		}
		if o.Memory > 0 && o.Memory < 128 {
			out = append(out, Finding{ID: "php.opcache-memory", Category: "php", Severity: SevLow,
				Title: "OPcache has " + strconv.Itoa(o.Memory) + " MB",
				Why:   "A Laravel app with its vendor/ usually needs 128–256 MB of opcode cache; a full cache restarts and recompiles.",
				Fix:   "opcache.memory_consumption=256"})
		}
		out = append(out, Finding{ID: "php.opcache-prod", Category: "php", Severity: SevInfo,
			Title: "Production OPcache settings",
			Why:   "In production code only changes on deploy, so OPcache shouldn't check file timestamps (reset it as part of the deploy: reload php-fpm or opcache_reset()). Your local PHP keeps checking them so edits show — production-like mode turns that off for this site.",
			Fix:   "opcache.enable=1\nopcache.validate_timestamps=0\nopcache.memory_consumption=256\nopcache.interned_strings_buffer=16\nopcache.max_accelerated_files=" + strconv.Itoa(nextOpcacheSize(max(f.PHPFiles, 10000)))})
	}
	return out
}

// nextOpcacheSize rounds a file count up to one of the primes OPcache
// uses internally (it rounds max_accelerated_files up to them anyway).
func nextOpcacheSize(n int) int {
	for _, p := range []int{223, 463, 983, 1979, 3907, 7963, 16229, 32531, 65407, 130987, 262237, 524521, 1048793} {
		if p >= n {
			return p
		}
	}
	return 1048793
}

func majorMinor(v string) string {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'()`)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func uniq(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		// "GET|HEAD /x" and "GET /x" are the same route.
		k := s
		if m, rest, ok := strings.Cut(s, " "); ok {
			m, _, _ = strings.Cut(m, "|")
			k = m + " " + rest
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

func capList(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return append(list[:n:n], "… and "+strconv.Itoa(len(list)-n)+" more")
}

// envCall matches env('X') / env("X", …) outside a comment.
var envCall = regexp.MustCompile(`(^|[^\w>:$\\])env\(\s*['"]`)

// FindEnvCalls scans PHP sources for env() calls; files maps a
// project-relative path to its content (the caller skips config/).
func FindEnvCalls(files map[string]string) []SourceHit {
	var out []SourceHit
	for path, text := range files {
		for i, l := range strings.Split(text, "\n") {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "#") {
				continue
			}
			if envCall.MatchString(l) {
				if len(t) > 140 {
					t = t[:140] + "…"
				}
				out = append(out, SourceHit{File: path, Line: i + 1, Text: t})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// SortFindings orders by severity, then category and id.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Ignored != fs[j].Ignored {
			return !fs[i].Ignored
		}
		if a, b := sevRank[fs[i].Severity], sevRank[fs[j].Severity]; a != b {
			return a < b
		}
		if fs[i].Category != fs[j].Category {
			return fs[i].Category < fs[j].Category
		}
		return fs[i].ID < fs[j].ID
	})
}

// MarkIgnored flags the findings the user dismissed.
func MarkIgnored(fs []Finding, ignored []string) {
	set := map[string]bool{}
	for _, id := range ignored {
		set[id] = true
	}
	for i := range fs {
		fs[i].Ignored = set[fs[i].ID]
	}
}
