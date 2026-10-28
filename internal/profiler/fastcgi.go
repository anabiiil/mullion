package profiler

// What Caddy passes to php-fpm for a site (its php_fastcgi block's env
// lines). SPX's settings are PHP_INI_SYSTEM, so they go in
// PHP_ADMIN_VALUE — php-fpm applies it per request, after PHP_VALUE
// (where the live watcher puts its prepend), and before SPX reads its
// settings at request startup.
//
// php-fpm never resets a FastCGI-passed ini value afterwards: the worker
// keeps it for whatever site it serves next. So once any site of a PHP
// version is profiled, every other site of that version gets the "off"
// values too (spx.http_enabled=0…), and when a gated site moved
// auto_prepend_file to the gate, the others get theirs back.

import (
	"path/filepath"
	"strings"

	"pm/internal/pmdir"
)

// EnvVar is one `env NAME "value"` line of a php_fastcgi block.
type EnvVar struct {
	Name  string
	Value string
}

// Pool describes the PHP version (php-fpm pool) a site runs on.
type Pool struct {
	// SpxLoaded: the version's php.ini loads spx.so (phpver.SpxLoaded).
	// Without it nothing is passed.
	SpxLoaded bool
	// Profiled: some site on this version is profiled.
	Profiled bool
	// Gated: some site on this version uses the gate (auto_prepend_file).
	Gated bool
	// IniPrepend is php.ini's own auto_prepend_file ("" = none).
	IniPrepend string
}

// SiteOpts is the rest of what one site's lines depend on.
type SiteOpts struct {
	// WatchPrepend is the live watcher's prepend when the site has it on
	// (it comes in PHP_VALUE; "" = off).
	WatchPrepend string
}

// iniQuote quotes an ini value (php's ini scanner reads "…" verbatim
// apart from \" and \\).
func iniQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

func boolIni(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// AdminValue renders the PHP_ADMIN_VALUE text (ini lines) for a site:
// its SPX settings when s is non-nil, else the "off" values its pool
// needs (see the file comment); "" when nothing has to be passed.
func AdminValue(paths pmdir.Paths, site string, s *Site, pool Pool, opts SiteOpts) string {
	if !pool.SpxLoaded {
		return ""
	}
	var lines []string
	add := func(k, v string) { lines = append(lines, k+"="+v) }
	if s == nil {
		if !pool.Profiled {
			return ""
		}
		add("spx.http_enabled", "0")
		add("spx.http_profiling_enabled", "0")
	} else {
		add("spx.http_enabled", "1")
		add("spx.http_key", iniQuote(s.Key))
		add("spx.http_ip_whitelist", iniQuote("127.0.0.1,::1"))
		add("spx.data_dir", iniQuote(filepath.ToSlash(ReportsDir(paths, site))))
		add("spx.http_profiling_builtins", boolIni(s.Builtins))
		switch {
		case s.Mode == ModeAll:
			add("spx.http_profiling_enabled", "1")
			add("spx.http_profiling_auto_start", "1")
		case s.Mode.usesGate():
			add("spx.http_profiling_enabled", "1")
			add("spx.http_profiling_auto_start", "0")
			add("auto_prepend_file", iniQuote(filepath.ToSlash(GatePath(paths))))
		default: // browser: the cookie / query string decides
			add("spx.http_profiling_enabled", "0")
			add("spx.http_profiling_auto_start", "1")
		}
	}
	// Give the site its own prepend back after a gated site's worker.
	if pool.Gated && opts.WatchPrepend == "" && (s == nil || !s.Mode.usesGate()) {
		add("auto_prepend_file", iniQuote(pool.IniPrepend))
	}
	return strings.Join(lines, "\n")
}

// Env is the site's full set of php_fastcgi env lines (nil = none).
func Env(paths pmdir.Paths, site string, s *Site, pool Pool, opts SiteOpts) []EnvVar {
	v := AdminValue(paths, site, s, pool, opts)
	if v == "" {
		return nil
	}
	env := []EnvVar{{Name: "PHP_ADMIN_VALUE", Value: v}}
	if s != nil && s.Mode.usesGate() {
		env = append(env, EnvVar{Name: "MULLION_SPX", Value: filepath.ToSlash(gateConfigPath(paths, site))})
		chain := opts.WatchPrepend
		if chain == "" {
			chain = pool.IniPrepend
		}
		if chain != "" {
			env = append(env, EnvVar{Name: "MULLION_SPX_CHAIN", Value: chain})
		}
	}
	return env
}
