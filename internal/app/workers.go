package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/nodever"
	"pm/internal/workers"
)

// WorkerView is one worker as the panel shows it: its config plus live
// status.
type WorkerView struct {
	config.Worker
	Status workers.WorkerStatus `json:"status"`
}

// SiteEnv is the environment a site's commands run in (workers, project
// commands, a terminal): the current environment with PATH led by the
// site's PHP, its Node version (when the project uses Node) and
// Mullion's bin dir (composer, mullion), so `php`, `composer`, `npm` and
// `node` resolve to exactly what serves the site; PHPRC points at the
// site's PHP so its php.ini (extensions, limits) applies.
func (a *App) SiteEnv(site config.Site) []string {
	var dirs []string
	env := os.Environ()
	// Frontend sites get the global PHP too (monorepo scripts call php).
	if v := a.SiteVersion(site); v != "" {
		phpDir := a.Paths.PhpVersionDir(v)
		dirs = append(dirs, phpDir)
		// The static PHP builds only read the php.ini next to them when
		// told to (the shell profile's PHPRC points at php/current —
		// wrong for a site pinned to another version).
		env = setEnv(env, "PHPRC", phpDir)
	}
	if siteUsesNode(site) {
		// Before BinDir: it holds node/npm/npx shims for the GLOBAL Node.
		if nodeDir, err := a.NodeVersionDirFor(site); err == nil {
			dirs = append(dirs, nodever.BinDir(nodeDir))
		}
		// Let server-side Node code trust Caddy's local CA for calls to
		// other https://*.test sites (same as the managed dev servers).
		if os.Getenv("NODE_EXTRA_CA_CERTS") == "" {
			if _, err := os.Stat(caddy.RootCertPath()); err == nil {
				env = append(env, "NODE_EXTRA_CA_CERTS="+caddy.RootCertPath())
			}
		}
	}
	dirs = append(dirs, a.Paths.BinDir())
	return prependPath(env, dirs)
}

// setEnv replaces (or adds) one variable in env.
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == key || (runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

func siteUsesNode(site config.Site) bool {
	if site.Kind == "node" {
		return true
	}
	_, err := os.Stat(filepath.Join(site.Path, "package.json"))
	return err == nil
}

// prependPath returns env with dirs put first on PATH (matching the
// variable's name case-insensitively on Windows, where it is "Path"),
// dropping later duplicates of those dirs.
func prependPath(env []string, dirs []string) []string {
	fold := runtime.GOOS == "windows"
	isPath := func(kv string) (string, bool) {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return "", false
		}
		k := kv[:i]
		if k == "PATH" || (fold && strings.EqualFold(k, "PATH")) {
			return k, true
		}
		return "", false
	}
	key, current, at := "PATH", "", -1
	for i, kv := range env {
		if k, ok := isPath(kv); ok {
			key, current, at = k, kv[len(k)+1:], i
		}
	}
	seen := map[string]bool{}
	var parts []string
	add := func(d string) {
		if d == "" {
			return
		}
		norm := filepath.Clean(d)
		if fold {
			norm = strings.ToLower(norm)
		}
		if seen[norm] {
			return
		}
		seen[norm] = true
		parts = append(parts, d)
	}
	for _, d := range dirs {
		add(d)
	}
	for _, d := range filepath.SplitList(current) {
		add(d)
	}
	entry := key + "=" + strings.Join(parts, string(os.PathListSeparator))
	out := make([]string, 0, len(env)+1)
	for i, kv := range env {
		if _, ok := isPath(kv); ok {
			if i == at {
				out = append(out, entry)
			}
			continue
		}
		out = append(out, kv)
	}
	if at < 0 {
		out = append(out, entry)
	}
	return out
}

func (a *App) siteOrErr(name string) (*config.Site, error) {
	site := a.State.FindSite(name)
	if site == nil {
		return nil, fmt.Errorf("no site named %q", name)
	}
	return site, nil
}

func findWorker(site *config.Site, id string) (int, error) {
	for i := range site.Workers {
		if site.Workers[i].ID == id {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%s has no worker %q", site.Name, id)
}

var workerKinds = map[string]bool{"queue": true, "scheduler": true, "custom": true}

func normalizeWorker(w *config.Worker) error {
	w.Name = strings.TrimSpace(w.Name)
	w.Command = strings.TrimSpace(w.Command)
	w.Kind = strings.ToLower(strings.TrimSpace(w.Kind))
	if w.Command == "" {
		return fmt.Errorf("a worker needs a command")
	}
	if strings.ContainsAny(w.Command, "\r\n") {
		return fmt.Errorf("a worker command must be a single line")
	}
	if w.Kind == "" {
		w.Kind = "custom"
	}
	if !workerKinds[w.Kind] {
		return fmt.Errorf("unknown worker kind %q (queue, scheduler or custom)", w.Kind)
	}
	if w.Name == "" {
		w.Name = w.Command
	}
	return nil
}

func newWorkerID(site *config.Site, name string) string {
	base := config.Slugify(name)
	if len(base) > 24 {
		base = strings.Trim(base[:24], "-")
	}
	if base == "" {
		base = "worker"
	}
	for {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		id := base + "-" + hex.EncodeToString(b)
		if _, err := findWorker(site, id); err != nil {
			return id
		}
	}
}

// AddWorker adds a worker to a site (a fresh ID from its name) and, when
// it is AutoStart, starts it right away.
func (a *App) AddWorker(siteName string, w config.Worker) (config.Worker, error) {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return config.Worker{}, err
	}
	if err := normalizeWorker(&w); err != nil {
		return config.Worker{}, err
	}
	w.ID = newWorkerID(site, w.Name)
	w.Paused = false
	site.Workers = append(site.Workers, w)
	if err := a.State.Save(); err != nil {
		return config.Worker{}, err
	}
	if w.AutoStart {
		if err := workers.Start(a.Paths, *site, w, a.SiteEnv(*site)); err != nil {
			return w, err
		}
	}
	return w, nil
}

// UpdateWorker changes a worker's name, kind, command or AutoStart (ID
// and Paused are kept). A running worker is restarted when its command
// changed.
func (a *App) UpdateWorker(siteName string, w config.Worker) error {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return err
	}
	i, err := findWorker(site, w.ID)
	if err != nil {
		return err
	}
	if err := normalizeWorker(&w); err != nil {
		return err
	}
	old := site.Workers[i]
	w.Paused = old.Paused
	site.Workers[i] = w
	if err := a.State.Save(); err != nil {
		return err
	}
	if old.Command != w.Command && workers.Running(a.Paths, site.Name, w.ID) {
		if err := workers.Stop(a.Paths, site.Name, w.ID); err != nil {
			return err
		}
		return workers.Start(a.Paths, *site, w, a.SiteEnv(*site))
	}
	return nil
}

// RemoveWorker stops a worker and deletes it (and its log).
func (a *App) RemoveWorker(siteName, id string) error {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return err
	}
	i, err := findWorker(site, id)
	if err != nil {
		return err
	}
	// Drop it from the config FIRST: stopping it while it is still
	// configured would let a supervisor pass auto-start it again.
	site.Workers = append(site.Workers[:i], site.Workers[i+1:]...)
	if err := a.State.Save(); err != nil {
		return err
	}
	return workers.Forget(a.Paths, site.Name, id)
}

// StartWorker unpauses a worker and starts it now (clearing any
// crash-loop mark).
func (a *App) StartWorker(siteName, id string) error {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return err
	}
	i, err := findWorker(site, id)
	if err != nil {
		return err
	}
	if site.Workers[i].Paused {
		site.Workers[i].Paused = false
		if err := a.State.Save(); err != nil {
			return err
		}
	}
	return workers.Start(a.Paths, *site, site.Workers[i], a.SiteEnv(*site))
}

// StopWorker pauses a worker (so the supervisor leaves it down) and
// stops it.
func (a *App) StopWorker(siteName, id string) error {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return err
	}
	i, err := findWorker(site, id)
	if err != nil {
		return err
	}
	if !site.Workers[i].Paused {
		site.Workers[i].Paused = true
		if err := a.State.Save(); err != nil {
			return err
		}
	}
	return workers.Stop(a.Paths, site.Name, id)
}

// RestartWorker stops and starts a worker (unpausing it) — picks up
// code/.env changes, which long-running PHP workers never reload.
func (a *App) RestartWorker(siteName, id string) error {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return err
	}
	if _, err := findWorker(site, id); err != nil {
		return err
	}
	if err := workers.Stop(a.Paths, site.Name, id); err != nil {
		return err
	}
	return a.StartWorker(siteName, id)
}

// WorkersStatus lists a site's workers with their live status (nil for
// an unknown site).
func (a *App) WorkersStatus(siteName string) []WorkerView {
	site := a.State.FindSite(siteName)
	if site == nil {
		return nil
	}
	out := make([]WorkerView, 0, len(site.Workers))
	for _, w := range site.Workers {
		out = append(out, WorkerView{Worker: w, Status: workers.Status(a.Paths, *site, w)})
	}
	return out
}

// WorkerLog returns the last lines of a worker's log.
func (a *App) WorkerLog(siteName, id string, lines int) string {
	site := a.State.FindSite(siteName)
	if site == nil {
		return ""
	}
	return workers.LogTail(a.Paths, site.Name, id, lines)
}

// WorkerTemplates suggests workers for a project: framework is the
// lowercase framework key ("laravel", "symfony", "nuxt", "next", "vite",
// "node", "express", "nest", …) and dir the project folder (read to
// tailor the list). Templates have no ID; pass one to AddWorker.
func WorkerTemplates(framework, dir string) []config.Worker {
	var out []config.Worker
	framework = strings.ToLower(strings.TrimSpace(framework))
	composer := readComposer(dir)
	switch framework {
	case "laravel":
		out = append(out, config.Worker{
			Name: "Queue worker", Kind: "queue", AutoStart: true,
			Command: "php artisan queue:work --tries=3 --sleep=1",
		})
		out = append(out, config.Worker{
			Name: "Scheduler", Kind: "scheduler", AutoStart: true,
			Command: laravelScheduleCommand(laravelMajor(dir)),
		})
		if composer.has("laravel/horizon") {
			out = append(out, config.Worker{
				Name: "Horizon", Kind: "queue", AutoStart: true,
				Command: "php artisan horizon",
			})
		}
		if composer.has("laravel/reverb") {
			out = append(out, config.Worker{
				Name: "Reverb", Kind: "custom", AutoStart: true,
				Command: "php artisan reverb:start",
			})
		}
	case "symfony":
		if composer.missing || composer.has("symfony/messenger") {
			out = append(out, config.Worker{
				Name: "Messenger consumer", Kind: "queue", AutoStart: true,
				Command: "php bin/console messenger:consume async -vv",
			})
		}
	}
	// Long-running package.json scripts, for any project that has them
	// (the dev server itself is managed separately, so "dev" is left out).
	if scripts := readPackageScripts(dir); len(scripts) > 0 {
		pm := packageManager(dir)
		names := make([]string, 0, len(scripts))
		for name := range scripts {
			if looksLongRunning(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			kind := "custom"
			lower := strings.ToLower(name)
			switch {
			case strings.Contains(lower, "queue") || strings.Contains(lower, "worker") || strings.Contains(lower, "jobs"):
				kind = "queue"
			case strings.Contains(lower, "cron") || strings.Contains(lower, "schedul"):
				kind = "scheduler"
			}
			out = append(out, config.Worker{
				Name: pm + " run " + name, Kind: kind,
				Command: pm + " run " + name,
			})
		}
	}
	return out
}

// laravelScheduleCommand: `schedule:work` (Laravel 8+) runs the
// scheduler every minute in the foreground — the cron entry, without
// cron. Older apps get an equivalent loop over schedule:run.
func laravelScheduleCommand(major int) string {
	if major == 0 || major >= 8 {
		return "php artisan schedule:work"
	}
	if runtime.GOOS == "windows" {
		return "for /L %i in () do @(php artisan schedule:run & ping -n 61 127.0.0.1 >nul)"
	}
	return "while true; do php artisan schedule:run; sleep 60; done"
}

var longRunningScript = []string{"start", "worker", "queue", "cron", "jobs", "schedul", "consume"}

func looksLongRunning(script string) bool {
	s := strings.ToLower(script)
	if strings.HasPrefix(s, "pre") || strings.HasPrefix(s, "post") {
		return false // npm lifecycle hooks (prestart, poststart)
	}
	for _, w := range longRunningScript {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// laravelMajor reads laravel/framework's major version from
// composer.lock (0 = unknown).
func laravelMajor(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, "composer.lock"))
	if err != nil {
		return 0
	}
	var lock struct {
		Packages []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return 0
	}
	for _, p := range lock.Packages {
		if p.Name == "laravel/framework" {
			v := strings.TrimPrefix(p.Version, "v")
			if i := strings.IndexByte(v, '.'); i > 0 {
				v = v[:i]
			}
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}

type composerInfo struct {
	missing bool
	deps    map[string]bool
	scripts map[string]string
}

func (c composerInfo) has(pkg string) bool { return c.deps[pkg] }

func readComposer(dir string) composerInfo {
	info := composerInfo{deps: map[string]bool{}, scripts: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(dir, "composer.json"))
	if err != nil {
		info.missing = true
		return info
	}
	var c struct {
		Require    map[string]any             `json:"require"`
		RequireDev map[string]any             `json:"require-dev"`
		Scripts    map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal(data, &c) != nil {
		return info
	}
	for k := range c.Require {
		info.deps[strings.ToLower(k)] = true
	}
	for k := range c.RequireDev {
		info.deps[strings.ToLower(k)] = true
	}
	for name, raw := range c.Scripts {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			info.scripts[name] = one
		case json.Unmarshal(raw, &many) == nil:
			info.scripts[name] = strings.Join(many, " && ")
		default:
			info.scripts[name] = ""
		}
	}
	return info
}

func readPackageScripts(dir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	return pkg.Scripts
}

// packageManager picks npm/pnpm/yarn/bun from the project's lockfile.
func packageManager(dir string) string {
	for _, c := range []struct{ file, pm string }{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
	} {
		if _, err := os.Stat(filepath.Join(dir, c.file)); err == nil {
			return c.pm
		}
	}
	return "npm"
}
