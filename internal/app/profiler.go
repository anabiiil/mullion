package app

// The project page's Performance → Profiler tab (and `mullion profile`):
// turning SPX on for a site, the php_fastcgi env lines Caddy needs for
// it (ProfilerFastCGIEnv), and the site's stored profiles.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/config"
	"pm/internal/fcgi"
	"pm/internal/phpver"
	"pm/internal/profiler"
	"pm/internal/watch"
)

// ProfilerInfo is the Profiler tab's header.
type ProfilerInfo struct {
	Site string `json:"site"`
	// Path is the project folder (the panel shortens file names to it).
	Path string `json:"path"`
	// Supported: this platform can build SPX (macOS); Note says why not.
	Supported bool   `json:"supported"`
	Note      string `json:"note,omitempty"`
	PHP       string `json:"php"`
	// SpxLoaded: the site's PHP version loads spx.so.
	SpxLoaded  bool   `json:"spxLoaded"`
	SpxVersion string `json:"spxVersion"`
	// Enabled: the site is profiled (Settings set).
	Enabled  bool           `json:"enabled"`
	Settings *profiler.Site `json:"settings,omitempty"`
	// Gate is the "next N"/"path" counter (captured so far, left).
	Gate *profiler.GateState `json:"gate,omitempty"`
	// Finished: a "next N" run captured all N and was switched back to
	// browser-triggered profiling.
	Finished bool `json:"finished,omitempty"`
	// Wired: the Caddyfile passes the site's SPX settings to PHP.
	Wired bool `json:"wired"`
	// ControlURL opens SPX's control panel on the site (it sets the
	// browser cookies); ProfileURL profiles one request of the home page.
	ControlURL string          `json:"controlUrl,omitempty"`
	ProfileURL string          `json:"profileUrl,omitempty"`
	ReportURL  string          `json:"reportUrl,omitempty"` // + report id
	Reports    int             `json:"reports"`
	Bytes      int64           `json:"bytes"`
	Limits     profiler.Limits `json:"limits"`
	Dir        string          `json:"dir"`
}

// profilerSite finds a PHP site.
func (a *App) profilerSite(name string) (config.Site, error) {
	s := a.State.FindSite(name)
	if s == nil {
		return config.Site{}, fmt.Errorf("no site named %q", name)
	}
	if !s.IsPHP() {
		return config.Site{}, fmt.Errorf("%s is not a PHP site: the profiler profiles PHP requests", s.Name)
	}
	return *s, nil
}

// siteBaseURL is the site's own address.
func (a *App) siteBaseURL(s config.Site) string {
	scheme := "http"
	if s.Secure {
		scheme = "https"
	}
	return scheme + "://" + a.State.Host(s)
}

// ProfilerStatus describes a site's profiler. A "next N" run that has
// captured all N is switched back to browser-triggered profiling here
// (the gate would only keep SPX's hooks armed for nothing).
func (a *App) ProfilerStatus(name string) (ProfilerInfo, error) {
	s, err := a.profilerSite(name)
	if err != nil {
		return ProfilerInfo{}, err
	}
	st, err := profiler.LoadState(a.Paths)
	if err != nil {
		return ProfilerInfo{}, err
	}
	version := a.SiteVersion(s)
	info := ProfilerInfo{
		Site: s.Name, Path: s.Path, Supported: phpver.SpxSupported(), PHP: version,
		SpxVersion: phpver.SpxVersion, Limits: st.Limits,
		Dir: profiler.ReportsDir(a.Paths, s.Name),
	}
	if !info.Supported {
		info.Note = phpver.EnsureSpx(context.Background(), a.Paths, version).Error()
	}
	if version != "" {
		info.SpxLoaded = phpver.SpxLoaded(a.Paths, version)
	}
	if ps := st.Get(s.Name); ps != nil {
		if ps.Mode == profiler.ModeNext {
			if g, ok := profiler.ReadGate(a.Paths, s.Name); ok && g.Remaining == 0 {
				ps.Mode = profiler.ModeBrowser
				if err := profiler.Prepare(a.Paths, s.Name, ps, true); err == nil && st.Save(a.Paths) == nil {
					info.Finished = true
					_ = a.applyIfEnabled()
				}
			}
		}
		info.Enabled = true
		info.Settings = ps
		if ps.Mode == profiler.ModeNext || ps.Mode == profiler.ModePath {
			if g, ok := profiler.ReadGate(a.Paths, s.Name); ok {
				info.Gate = &g
			}
		}
		base := a.siteBaseURL(s)
		k := url.QueryEscape(ps.Key)
		info.ControlURL = base + "/?SPX_KEY=" + k + "&SPX_UI_URI=/"
		info.ProfileURL = base + "/?SPX_ENABLED=1&SPX_KEY=" + k
		info.ReportURL = base + "/?SPX_KEY=" + k + "&SPX_UI_URI=/report.html&key="
		if data, err := os.ReadFile(a.Paths.Caddyfile()); err == nil {
			info.Wired = strings.Contains(string(data), ps.Key)
		}
	}
	info.Reports, info.Bytes = profiler.Usage(info.Dir)
	return info, nil
}

// SetProfiler turns profiling on (with settings s) or off for a site.
// Turning it on builds SPX for the site's PHP version first when needed
// (a minute or two, logged through log) and restarts that version's
// php-fpm so it loads spx.so. Off keeps the reports, and spx.so stays
// loaded (disabled, it costs nothing).
func (a *App) SetProfiler(ctx context.Context, name string, on bool, s profiler.Site, log func(string)) (ProfilerInfo, error) {
	site, err := a.profilerSite(name)
	if err != nil {
		return ProfilerInfo{}, err
	}
	st, err := profiler.LoadState(a.Paths)
	if err != nil {
		return ProfilerInfo{}, err
	}
	if !on {
		if st.Get(site.Name) != nil {
			delete(st.Sites, site.Name)
			if err := st.Save(a.Paths); err != nil {
				return ProfilerInfo{}, err
			}
			_ = profiler.Prepare(a.Paths, site.Name, &profiler.Site{Mode: profiler.ModeBrowser}, true)
			if err := a.applyIfEnabled(); err != nil {
				return ProfilerInfo{}, err
			}
		}
		return a.ProfilerStatus(site.Name)
	}

	version := a.SiteVersion(site)
	if version == "" {
		return ProfilerInfo{}, errors.New("no PHP version set: run `mullion use <version>` first")
	}
	if !phpver.SpxSupported() {
		return ProfilerInfo{}, phpver.EnsureSpx(ctx, a.Paths, version)
	}
	wasLoaded := phpver.SpxLoaded(a.Paths, version)
	if !wasLoaded && log != nil {
		log(fmt.Sprintf("Building the SPX %s profiler for PHP %s…", phpver.SpxVersion, version))
	}
	if err := phpver.EnsureSpx(ctx, a.Paths, version); err != nil {
		return ProfilerInfo{}, err
	}

	if prev := st.Get(site.Name); prev != nil {
		s.Key = prev.Key
		if s.Since == 0 {
			s.Since = prev.Since
		}
	}
	if err := s.Validate(); err != nil {
		return ProfilerInfo{}, err
	}
	if err := profiler.Prepare(a.Paths, site.Name, &s, true); err != nil {
		return ProfilerInfo{}, err
	}
	st.Sites[site.Name] = &s
	if err := st.Save(a.Paths); err != nil {
		return ProfilerInfo{}, err
	}
	// php-fpm loads php.ini once: a pool started before spx.so was
	// added must restart (Apply's Ensure brings it back).
	if !wasLoaded && !a.skipApply {
		_ = fcgi.StopVersion(a.Paths, version)
	}
	if log != nil {
		log("Restarting PHP and Caddy for " + site.Name + "…")
	}
	if err := a.applyIfEnabled(); err != nil {
		return ProfilerInfo{}, err
	}
	return a.ProfilerStatus(site.Name)
}

// ProfilerFastCGIEnv is the site's extra php_fastcgi env lines (name,
// value) for the profiler — its SPX settings, or the "off" values when
// another site on its PHP version is profiled (php-fpm keeps passed ini
// values per worker; see internal/profiler/fastcgi.go). nil when nothing
// is needed, which is always the case until a site turns profiling on.
// WriteCaddyfile passes them to caddy.SiteConf for PHP sites.
func (a *App) ProfilerFastCGIEnv(s config.Site, version string) [][2]string {
	if !s.IsPHP() || version == "" || !phpver.SpxLoaded(a.Paths, version) {
		return nil
	}
	st, err := profiler.LoadState(a.Paths)
	if err != nil || len(st.Sites) == 0 {
		return nil
	}
	pool := profiler.Pool{SpxLoaded: true}
	for _, o := range a.State.Sites {
		if !o.IsPHP() || a.SiteVersion(o) != version {
			continue
		}
		if ps := st.Get(o.Name); ps != nil {
			pool.Profiled = true
			if ps.Mode == profiler.ModeNext || ps.Mode == profiler.ModePath {
				pool.Gated = true
			}
		}
	}
	pool.IniPrepend, _ = phpver.IniDirective(phpver.IniPath(a.Paths, version), "auto_prepend_file")
	var opts profiler.SiteOpts
	if s.WatchPrepend() {
		opts.WatchPrepend = filepath.ToSlash(watch.GatePath(a.Paths))
	}
	ps := st.Get(s.Name)
	if ps != nil {
		// The reports dir and gate must exist before PHP is pointed at
		// them; without them the site is served unprofiled.
		if err := profiler.Prepare(a.Paths, s.Name, ps, false); err != nil {
			ps = nil
		}
	}
	var out [][2]string
	for _, e := range profiler.Env(a.Paths, s.Name, ps, pool, opts) {
		out = append(out, [2]string{e.Name, e.Value})
	}
	return out
}

// ProfilerReports lists a site's stored profiles, newest first, after
// trimming them to the disk limits.
func (a *App) ProfilerReports(name string) ([]profiler.Meta, error) {
	s, err := a.profilerSite(name)
	if err != nil {
		return nil, err
	}
	st, err := profiler.LoadState(a.Paths)
	if err != nil {
		return nil, err
	}
	dir := profiler.ReportsDir(a.Paths, s.Name)
	_, _ = profiler.Enforce(dir, st.Limits)
	return profiler.List(dir)
}

// ProfilerReport parses one stored profile; frames below minFrac of the
// total are pruned from its flame graph (0 = keep everything).
func (a *App) ProfilerReport(name, id string, minFrac float64) (profiler.Meta, *profiler.Profile, error) {
	s, err := a.profilerSite(name)
	if err != nil {
		return profiler.Meta{}, nil, err
	}
	m, p, err := profiler.LoadProfile(profiler.ReportsDir(a.Paths, s.Name), id)
	if err != nil {
		return m, nil, err
	}
	p.Prune(minFrac)
	return m, p, nil
}

// DeleteProfilerReport removes one profile ("" id = all of them) and
// returns how many went.
func (a *App) DeleteProfilerReport(name, id string) (int, error) {
	s, err := a.profilerSite(name)
	if err != nil {
		return 0, err
	}
	dir := profiler.ReportsDir(a.Paths, s.Name)
	if id == "" {
		return profiler.Clear(dir)
	}
	if err := profiler.Delete(dir, id); err != nil {
		return 0, err
	}
	return 1, nil
}

// SetProfilerLimits changes how many profiles (and bytes) a site keeps.
func (a *App) SetProfilerLimits(lim profiler.Limits) error {
	st, err := profiler.LoadState(a.Paths)
	if err != nil {
		return err
	}
	if lim.MaxReports > 0 {
		st.Limits.MaxReports = lim.MaxReports
	}
	if lim.MaxBytes > 0 {
		st.Limits.MaxBytes = lim.MaxBytes
	}
	return st.Save(a.Paths)
}
