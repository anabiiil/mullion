package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/frankenphp"
)

// The FrankenPHP hooks that download or start and stop real processes:
// variables, so this package's tests swap in fakes and never spawn one.
var (
	ensureFranken  = frankenphp.Ensure
	stopFranken    = frankenphp.Stop
	latestFranken  = frankenphp.Latest
	installFranken = frankenphp.Install
)

// frankenServer is the config value of a site on FrankenPHP.
const frankenServer = "frankenphp"

// errOctaneMissing is worker mode's refusal without Laravel Octane.
var errOctaneMissing = errors.New("worker mode needs Laravel Octane — run: composer require laravel/octane")

// SetSiteServer switches a PHP site between the shared php-fpm ("" or
// "fpm") and its own FrankenPHP server ("frankenphp"), worker mode
// (Laravel Octane) included. The first switch installs the latest
// FrankenPHP when none is installed yet.
func (a *App) SetSiteServer(ctx context.Context, name, server string, worker bool, progress func(string)) error {
	site := a.State.FindSite(name)
	if site == nil {
		return fmt.Errorf("no site named %q", name)
	}
	if !site.IsPHP() {
		return fmt.Errorf("only PHP sites can run on FrankenPHP")
	}
	switch strings.ToLower(strings.TrimSpace(server)) {
	case "", "fpm", "php-fpm":
		if worker {
			return fmt.Errorf("worker mode is a FrankenPHP option — run: mullion server %s frankenphp --worker", site.Name)
		}
		stopFranken(a.Paths, site.Name)
		site.Server = ""
		site.Franken = nil
		return a.applyIfEnabled()
	case frankenServer:
	default:
		return fmt.Errorf("unknown server %q: use fpm or frankenphp", server)
	}
	if worker && !octaneInstalled(site.Path) {
		return errOctaneMissing
	}

	if site.OnFranken() && site.Franken != nil && site.Franken.Tag != "" && site.Franken.Port > 0 {
		site.Franken.Worker = worker // keep the pinned tag and the port
		return a.applyIfEnabled()
	}
	installed := frankenphp.Installed(a.Paths)
	if len(installed) == 0 {
		rel, err := latestFranken(ctx)
		if err != nil {
			return err
		}
		if err := installFranken(ctx, a.Paths, rel, progress); err != nil {
			return err
		}
		if installed = frankenphp.Installed(a.Paths); len(installed) == 0 {
			return fmt.Errorf("FrankenPHP %s did not install", rel.Tag)
		}
	}
	site.Server = frankenServer
	site.Franken = &config.Franken{
		Tag:    installed[0],
		Worker: worker,
		Port:   frankenphp.AssignPort(a.usedFrankenPorts(site.Name)),
	}
	return a.applyIfEnabled()
}

// usedFrankenPorts is every port another site holds: dev-server ports
// and other FrankenPHP servers' ports.
func (a *App) usedFrankenPorts(except string) map[int]bool {
	used := map[int]bool{}
	for _, s := range a.State.Sites {
		if s.DevPort > 0 {
			used[s.DevPort] = true
		}
		if s.Franken != nil && s.Franken.Port > 0 && !strings.EqualFold(s.Name, except) {
			used[s.Franken.Port] = true
		}
	}
	return used
}

// UpdateFranken installs the latest FrankenPHP release and moves every
// FrankenPHP site onto it (their servers restart on the next Apply).
// It returns the new tag.
func (a *App) UpdateFranken(ctx context.Context, progress func(string)) (string, error) {
	rel, err := latestFranken(ctx)
	if err != nil {
		return "", err
	}
	have := false
	for _, t := range frankenphp.Installed(a.Paths) {
		have = have || t == rel.Tag
	}
	if !have {
		// Nothing should run a tag that isn't installed, but a leftover
		// would hold the folder Install replaces (Windows locks the exe).
		for _, s := range a.State.Sites {
			if s.OnFranken() && s.Franken != nil && s.Franken.Tag == rel.Tag {
				stopFranken(a.Paths, s.Name)
			}
		}
		if err := installFranken(ctx, a.Paths, rel, progress); err != nil {
			return "", err
		}
	}
	for i := range a.State.Sites {
		s := &a.State.Sites[i]
		if !s.OnFranken() || s.Franken == nil || s.Franken.Tag == rel.Tag {
			continue
		}
		stopFranken(a.Paths, s.Name)
		s.Franken.Tag = rel.Tag
	}
	return rel.Tag, a.applyIfEnabled()
}

// frankenSiteConf is the site's FrankenPHP server configuration: what
// php_fastcgi sites get in Mullion's main Caddyfile (watcher prepend,
// production-like env and OPcache) goes into the site's own instead.
func (a *App) frankenSiteConf(s config.Site) frankenphp.SiteConf {
	c := frankenphp.SiteConf{
		Name:          s.Name,
		Root:          caddy.DocRoot(s.Path),
		Secure:        s.Secure,
		FreezeOpcache: s.ProdMode(),
	}
	if s.Franken != nil {
		c.Port = s.Franken.Port
	}
	if w := a.watchCaddyConf(s); w != nil {
		c.Prepend, c.WatchDir = w.Prepend, w.Dir
	}
	if p := a.prodCaddyConf(s); p != nil {
		c.Env = p.Env
	}
	return c
}

// EnsureFrankenServers brings up the FrankenPHP server of every site on
// FrankenPHP and returns their ports. A site that fails to start is
// reported but does not block the others — its host serves a clear 503
// instead. Servers of sites no longer on FrankenPHP (unlinked, renamed,
// switched back to php-fpm) are stopped.
func (a *App) EnsureFrankenServers() map[string]int {
	ports := map[string]int{}
	keep := map[string]bool{}
	for _, s := range a.State.Sites {
		if s.OnFranken() {
			keep[strings.ToLower(s.Name)] = true
		}
	}
	for _, name := range frankenPidSites(a) {
		if !keep[strings.ToLower(name)] {
			stopFranken(a.Paths, name)
		}
	}
	for _, s := range a.State.Sites {
		if !s.OnFranken() {
			continue
		}
		if s.Franken == nil || s.Franken.Tag == "" || s.Franken.Port <= 0 {
			fmt.Printf("warning: %s: no FrankenPHP release or port set — run: mullion server %s frankenphp\n", s.Name, s.Name)
			continue
		}
		bin := frankenphp.Binary(a.Paths, s.Franken.Tag)
		if st, err := os.Stat(bin); err != nil || st.IsDir() {
			fmt.Printf("warning: %s: FrankenPHP %s is not installed — run: mullion frankenphp update\n", s.Name, s.Franken.Tag)
			continue
		}
		if !frankenphp.Running(a.Paths, s.Name) {
			a.progress("Starting " + s.Name + "'s FrankenPHP server…")
		}
		port, err := ensureFranken(frankenphp.Run{
			Paths:      a.Paths,
			Bin:        bin,
			Site:       a.frankenSiteConf(s),
			ProjectDir: s.Path,
			Worker:     s.Franken.Worker,
		})
		if err != nil {
			fmt.Printf("warning: %s: %v\n", s.Name, err)
			continue
		}
		ports[s.Name] = port
	}
	return ports
}

// frankenPidSites lists the sites that have a FrankenPHP pid file
// (frankenphp keeps them as <PidsDir>/franken-<site>.pid).
func frankenPidSites(a *App) []string {
	entries, err := os.ReadDir(a.Paths.PidsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "franken-") && strings.HasSuffix(n, ".pid") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(n, "franken-"), ".pid"))
		}
	}
	return out
}

// octaneInstalled reports whether the project's composer.lock lists
// laravel/octane.
func octaneInstalled(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "composer.lock"))
	if err != nil {
		return false
	}
	var lock struct {
		Packages    []struct{ Name string } `json:"packages"`
		PackagesDev []struct{ Name string } `json:"packages-dev"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return false
	}
	for _, p := range append(lock.Packages, lock.PackagesDev...) {
		if strings.EqualFold(p.Name, "laravel/octane") {
			return true
		}
	}
	return false
}

// FrankenInfo is a site's server choice and its FrankenPHP state.
type FrankenInfo struct {
	// Server is "fpm" or "frankenphp".
	Server          string   `json:"server"`
	Worker          bool     `json:"worker"`
	OctaneInstalled bool     `json:"octaneInstalled"`
	Running         bool     `json:"running"`
	Tag             string   `json:"tag,omitempty"`
	PHPVersion      string   `json:"phpVersion,omitempty"`
	Log             string   `json:"log,omitempty"`
	Installed       []string `json:"installed"`
	Port            int      `json:"port,omitempty"`
}

// FrankenInfo describes which server runs a site and, on FrankenPHP,
// the server's release, embedded PHP, port and log.
func (a *App) FrankenInfo(name string) (FrankenInfo, error) {
	site := a.State.FindSite(name)
	if site == nil {
		return FrankenInfo{}, fmt.Errorf("no site named %q", name)
	}
	info := FrankenInfo{
		Server:          "fpm",
		OctaneInstalled: octaneInstalled(site.Path),
		Installed:       frankenphp.Installed(a.Paths),
	}
	if info.Installed == nil {
		info.Installed = []string{}
	}
	if !site.OnFranken() {
		return info, nil
	}
	info.Server = frankenServer
	info.Running = frankenphp.Running(a.Paths, site.Name)
	info.Log = filepath.Join(a.Paths.LogsDir(), "franken-"+site.Name+".log")
	if f := site.Franken; f != nil {
		info.Worker, info.Tag, info.Port = f.Worker, f.Tag, f.Port
		if f.Tag != "" {
			bin := frankenphp.Binary(a.Paths, f.Tag)
			if _, err := os.Stat(bin); err == nil {
				info.PHPVersion, _ = frankenphp.PHPVersion(bin)
			}
		}
	}
	return info, nil
}
