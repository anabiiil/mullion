package app

// The project page's Watch tab (and `mullion watch`): turning a site's
// live watcher on and off, and what it needs from the rest of Mullion —
// the Caddyfile's per-site PHP_VALUE (caddy.WatchConf), the CLI's extra
// ini scan dir (SiteEnv) and the Mail tab's Mailpit list.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pm/internal/caddy"
	"pm/internal/config"
	"pm/internal/detect"
	"pm/internal/mailpit"
	"pm/internal/redis"
	"pm/internal/watch"
	"pm/internal/workers"
)

// watchCaddyConf is the site's Caddyfile watcher block (nil = off). It
// also makes sure the prepend files exist before Caddy points PHP at
// them.
func (a *App) watchCaddyConf(s config.Site) *caddy.WatchConf {
	if !s.WatchPrepend() {
		return nil
	}
	if err := watch.EnsureFiles(a.Paths); err != nil {
		return nil // no prepend file = no PHP_VALUE, never a broken site
	}
	_ = watch.WriteSiteConfig(a.Paths, s)
	return &caddy.WatchConf{
		Prepend:   filepath.ToSlash(watch.GatePath(a.Paths)),
		Dir:       a.Paths.WatchSiteDir(s.Name),
		RequestID: caddy.SupportsLogAppend(a.Paths),
	}
}

// watchCLIEnv is what SiteEnv adds for a watched site's commands.
func (a *App) watchCLIEnv(s config.Site, env []string) map[string]string {
	if watch.EnsureFiles(a.Paths) != nil {
		return nil
	}
	_ = watch.WriteSiteConfig(a.Paths, s)
	current := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PHP_INI_SCAN_DIR" || runtime.GOOS == "windows" && strings.EqualFold(k, "PHP_INI_SCAN_DIR") {
			current = v
		}
	}
	return watch.CLIEnv(a.Paths, s.Name, current)
}

// WatchInfo is the Watch tab's header.
type WatchInfo struct {
	Site     string         `json:"site"`
	Settings watch.Settings `json:"settings"`
	// PHP: requests/dumps/deep need a PHP site; Laravel: deep needs it.
	PHP     bool `json:"php"`
	Laravel bool `json:"laravel"`
	// Prepend is true when dumps/deep are on: the prepend loads through
	// Caddy's per-site PHP_VALUE (php-fpm) or, on Windows, php-cgi's
	// extra ini scan dir (see fcgi.SetWebWatch).
	Prepend   bool     `json:"prepend"`
	RequestID bool     `json:"requestId"`
	Editors   []string `json:"editors"`
	Dir       string   `json:"dir"`
	Note      string   `json:"note,omitempty"`
	Workers   int      `json:"workers"`
	// DataBytes is what the watcher keeps on disk for the site.
	DataBytes int64 `json:"dataBytes"`
	// Cleared is when each view was last cleared (unix seconds by item
	// type; "" = everything) — the panel hides its own copies too.
	Cleared map[string]float64 `json:"cleared"`
}

// WatchStatus describes a site's watcher.
func (a *App) WatchStatus(site string) (WatchInfo, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return WatchInfo{}, fmt.Errorf("no site named %q", site)
	}
	info := WatchInfo{
		Site:      s.Name,
		Settings:  watch.SettingsOf(*s),
		PHP:       s.IsPHP(),
		Laravel:   detect.Project(s.Path).Icon == "laravel",
		Prepend:   s.WatchPrepend(),
		Editors:   watch.AvailableEditors(),
		Dir:       a.Paths.WatchSiteDir(s.Name),
		Workers:   len(s.Workers),
		DataBytes: watch.DataSize(a.Paths, s.Name),
		Cleared:   map[string]float64{},
	}
	c := watch.LoadClear(a.Paths, s.Name)
	for k, v := range c.Types {
		info.Cleared[k] = v
	}
	if c.At > 0 {
		info.Cleared[""] = c.At
	}
	if info.Editors == nil {
		info.Editors = []string{}
	}
	if info.Prepend {
		info.RequestID = caddy.SupportsLogAppend(a.Paths)
	}
	return info, nil
}

// SetWatch saves a site's watcher settings. Turning dumps/deep on or
// off rewrites the Caddyfile (Apply) and restarts the site's running
// workers so they pick the prepend up (or drop it).
func (a *App) SetWatch(site string, w config.Watch) error {
	s := a.State.FindSite(site)
	if s == nil {
		return fmt.Errorf("no site named %q", site)
	}
	if !s.IsPHP() && (w.Dumps || w.Deep) {
		return errors.New("dumps and the deep watcher need a PHP site")
	}
	if w.Editor != "" && !contains(watch.Editors, w.Editor) {
		return fmt.Errorf("unknown editor %q", w.Editor)
	}
	for _, v := range []*int{&w.SlowRequestMs, &w.SlowQueryMs} {
		if *v < 0 || *v > 600000 {
			return errors.New("a threshold must be between 0 and 600000 ms")
		}
	}
	if w.NPlusOne != 0 && (w.NPlusOne < 2 || w.NPlusOne > 1000) {
		return errors.New("the N+1 threshold must be between 2 and 1000")
	}
	before := s.WatchPrepend()
	cp := w
	s.Watch = &cp
	if err := watch.EnsureFiles(a.Paths); err != nil {
		return err
	}
	if err := watch.WriteSiteConfig(a.Paths, *s); err != nil {
		return err
	}
	if s.WatchPrepend() == before {
		return a.State.Save()
	}
	err := a.applyIfEnabled()
	if a.skipApply {
		return err
	}
	for _, wk := range s.Workers {
		if workers.Running(a.Paths, s.Name, wk.ID) {
			if rerr := a.RestartWorker(s.Name, wk.ID); rerr != nil && err == nil {
				err = rerr
			}
		}
	}
	return err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ClearWatch empties a site's watcher history: its events files, and
// older lines of Caddy's access log and storage/logs/*.log stay hidden
// (after a reconnect or a panel restart too). With logs it also empties
// the project's storage/logs/*.log files and returns them.
func (a *App) ClearWatch(site string, logs bool) ([]string, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return nil, fmt.Errorf("no site named %q", site)
	}
	var emptied []string
	if logs {
		var err error
		if emptied, err = watch.ClearProjectLogs(s.Path); err != nil {
			return emptied, err
		}
	}
	return emptied, watch.Clear(a.Paths, s.Name, watch.Sources(a.Paths, *s, a.State.Host(*s))...)
}

// watchLogTypes are the item kinds read from storage/logs.
var watchLogTypes = map[string]bool{"log": true, "exception/file": true}

// ClearWatchTypes hides a site's older items of some kinds (one view of
// the Watch tab: "query", "exception/file"…).
func (a *App) ClearWatchTypes(site string, types []string) error {
	s := a.State.FindSite(site)
	if s == nil {
		return fmt.Errorf("no site named %q", site)
	}
	if len(types) == 0 {
		return errors.New("nothing to clear")
	}
	var sources []string
	for _, t := range types {
		if watchLogTypes[t] {
			sources = watch.ProjectLogFiles(s.Path)
			break
		}
	}
	return watch.ClearTypes(a.Paths, s.Name, types, sources...)
}

// WatchFeed opens a feed of the site's watcher sources.
func (a *App) WatchFeed(site string) (*watch.Feed, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return nil, fmt.Errorf("no site named %q", site)
	}
	return watch.NewFeed(a.Paths, *s, a.State.Host(*s)), nil
}

// WatchOpenFile opens a project file at a line in the user's editor.
func (a *App) WatchOpenFile(site, file string, line int) (string, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return "", fmt.Errorf("no site named %q", site)
	}
	return watch.OpenInEditor(s.Path, file, line, watch.SettingsOf(*s).Editor)
}

// WatchMailItem is one message of the Watch tab's Mail list.
type WatchMailItem struct {
	mailpit.Summary
	// Match says why it's this project's: "from" (the .env's
	// MAIL_FROM_ADDRESS) or "" (only shown with "all recent").
	Match string `json:"match"`
}

// WatchMail lists Mailpit's recent messages, marking the ones sent from
// this project's MAIL_FROM_ADDRESS. (With the deep watcher on, the
// panel also matches them against the "mail" events it recorded.)
func (a *App) WatchMail(ctx context.Context, site string) (map[string]any, error) {
	s := a.State.FindSite(site)
	if s == nil {
		return nil, fmt.Errorf("no site named %q", site)
	}
	from := ""
	if data, err := os.ReadFile(filepath.Join(s.Path, ".env")); err == nil {
		from = strings.ToLower(strings.TrimSpace(redis.ParseEnv(string(data))["MAIL_FROM_ADDRESS"]))
	}
	out := map[string]any{"installed": a.State.Config.Mailpit != "", "from": from, "messages": []WatchMailItem{}}
	if a.State.Config.Mailpit == "" {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	list, err := mailpit.Messages(cctx, 100)
	if err != nil {
		out["error"] = "Mail isn't running"
		return out, nil
	}
	msgs := make([]WatchMailItem, 0, len(list.Messages))
	for _, m := range list.Messages {
		it := WatchMailItem{Summary: m}
		if from != "" && m.From != nil && strings.EqualFold(m.From.Address, from) {
			it.Match = "from"
		}
		msgs = append(msgs, it)
	}
	out["messages"] = msgs
	out["running"] = true
	return out, nil
}
