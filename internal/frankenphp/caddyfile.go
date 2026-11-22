package frankenphp

import (
	"fmt"
	"sort"
	"strings"
)

// SiteConf is one site's FrankenPHP server as its Caddyfile needs it.
type SiteConf struct {
	Name string // site name (a comment in the file)
	Root string // absolute document root
	Port int    // loopback port the server listens on
	// Secure marks a site Mullion's main Caddy serves over https: the
	// per-site server only speaks plain HTTP on loopback, so PHP is told
	// the original scheme and port (HTTPS, SERVER_PORT) to build correct
	// absolute URLs.
	Secure bool
	// Prepend is Mullion's auto_prepend_file (the watcher); WatchDir its
	// events folder (MULLION_WATCH).
	Prepend, WatchDir string
	// Env are production-like mode's $_SERVER values (APP_ENV=production…).
	Env map[string]string
	// FreezeOpcache sets opcache.validate_timestamps=0 (production-like).
	FreezeOpcache bool
}

// Caddyfile renders the per-site FrankenPHP server's Caddyfile: plain HTTP
// on 127.0.0.1:Port behind Mullion's main Caddy, which terminates TLS and
// forwards the original Host (and X-Forwarded-* headers, trusted only from
// loopback) so framework host/scheme detection keeps working.
func Caddyfile(c SiteConf) string {
	var b strings.Builder
	header(&b, c)
	b.WriteString("{\n\tadmin off\n\tauto_https off\n")
	b.WriteString(trustedProxies)
	if ini := phpIni(c); len(ini) > 0 {
		b.WriteString("\tfrankenphp {\n")
		for _, l := range ini {
			b.WriteString("\t\t" + l + "\n")
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n\n")

	// "http://:PORT", not "http://127.0.0.1:PORT": a host in the address is
	// also a Host matcher, and main Caddy forwards the site's own Host
	// (blog.test), which would match nothing and get an empty 200. bind
	// keeps the listener on loopback, so the LAN can't reach it.
	b.WriteString(fmt.Sprintf("http://:%d {\n\tbind 127.0.0.1\n", c.Port))
	b.WriteString("\troot * " + quote(c.Root) + "\n")

	env := phpEnv(c)
	if len(env) == 0 {
		b.WriteString("\tphp_server\n")
	} else {
		b.WriteString("\tphp_server {\n")
		for _, e := range env {
			b.WriteString("\t\t" + e + "\n")
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// WorkerCaddyfile renders the Caddyfile worker mode hands Laravel Octane
// (`octane:frankenphp --caddyfile=…`). Without it Octane uses its own
// stub, which listens on every interface and knows nothing of the
// forwarded scheme, the watcher or production-like mode.
//
// It follows Octane's stub (src/Commands/stubs/Caddyfile, 2.x): Octane
// starts `frankenphp run -c <this file>` with environment variables that
// Caddy substitutes for the {$…} placeholders kept here — the worker
// script lives at {$APP_PUBLIC_PATH}/frankenphp-worker.php, the worker
// count and Octane's --watch go into the worker block, and the admin
// endpoint (which octane:status/reload/stop talk to) stays on the port
// Octane was given. What Octane would decide differently is fixed here:
// plain HTTP bound to 127.0.0.1 on the site's port, the same trusted
// proxies, php_ini and php_server env lines as classic mode, and no
// {$CADDY_GLOBAL_OPTIONS} (auto_https is simply off).
func WorkerCaddyfile(c SiteConf) string {
	var b strings.Builder
	header(&b, c)
	b.WriteString("{\n\tauto_https off\n")
	b.WriteString("\tadmin 127.0.0.1:{$CADDY_SERVER_ADMIN_PORT}\n")
	b.WriteString(trustedProxies)
	b.WriteString("\tfrankenphp {\n")
	for _, l := range phpIni(c) {
		b.WriteString("\t\t" + l + "\n")
	}
	b.WriteString("\t\tworker {\n")
	b.WriteString("\t\t\tfile \"{$APP_PUBLIC_PATH}/frankenphp-worker.php\"\n")
	b.WriteString("\t\t\t{$CADDY_SERVER_WORKER_DIRECTIVE}\n")
	b.WriteString("\t\t\t{$CADDY_SERVER_WATCH_DIRECTIVES}\n")
	b.WriteString("\t\t}\n\t}\n}\n\n")
	b.WriteString("{$CADDY_EXTRA_CONFIG}\n\n")

	// The same listener as classic mode (see Caddyfile), in place of
	// Octane's {$CADDY_SERVER_SERVER_NAME} ("http://:PORT", every
	// interface).
	b.WriteString(fmt.Sprintf("http://:%d {\n\tbind 127.0.0.1\n", c.Port))
	// Octane's log block: it reads the server's JSON log lines back to
	// print errors.
	b.WriteString("\tlog {\n\t\tlevel {$CADDY_SERVER_LOG_LEVEL}\n")
	b.WriteString("\t\tformat filter {\n\t\t\twrap {$CADDY_SERVER_LOGGER}\n")
	b.WriteString("\t\t\tfields {\n\t\t\t\trequest>uri query {\n\t\t\t\t\treplace authorization REDACTED\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t}\n\n")
	b.WriteString("\t@compressible not path /.well-known/mercure\n\n")
	b.WriteString("\troute {\n")
	b.WriteString("\t\troot * \"{$APP_PUBLIC_PATH}\"\n")
	b.WriteString("\t\tencode @compressible zstd br gzip\n")
	b.WriteString("\t\t{$CADDY_SERVER_EXTRA_DIRECTIVES}\n")
	b.WriteString("\t\tphp_server {\n")
	b.WriteString("\t\t\tindex frankenphp-worker.php\n")
	b.WriteString("\t\t\ttry_files {path} frankenphp-worker.php\n")
	b.WriteString("\t\t\tresolve_root_symlink\n")
	for _, e := range phpEnv(c) {
		b.WriteString("\t\t\t" + e + "\n")
	}
	b.WriteString("\t\t}\n\t}\n}\n")
	return b.String()
}

// trustedProxies lets the server believe X-Forwarded-* only from main
// Caddy on loopback.
const trustedProxies = "\tservers {\n\t\ttrusted_proxies static 127.0.0.1/32 ::1/128\n\t}\n"

func header(b *strings.Builder, c SiteConf) {
	b.WriteString("# Generated by mullion — do not edit; changes are overwritten.\n")
	if c.Name != "" {
		b.WriteString("# FrankenPHP server for " + strings.NewReplacer("\n", " ", "\r", " ").Replace(c.Name) + "\n")
	}
}

// phpIni is the global frankenphp block's php_ini lines.
func phpIni(c SiteConf) []string {
	var out []string
	if c.Prepend != "" {
		out = append(out, "php_ini auto_prepend_file "+quote(c.Prepend))
	}
	if c.FreezeOpcache {
		out = append(out, "php_ini opcache.validate_timestamps 0")
	}
	return out
}

// phpEnv is php_server's env lines: the original scheme and port, the
// watcher's folder and request id, and production-like mode's values.
func phpEnv(c SiteConf) []string {
	var env []string
	if c.Secure {
		env = append(env, "env HTTPS on", "env SERVER_PORT 443")
	}
	if c.WatchDir != "" {
		env = append(env, "env MULLION_WATCH "+quote(c.WatchDir))
		env = append(env, "env MULLION_RID {http.request.header.X-Mullion-Rid}")
	}
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, "env "+k+" "+quote(c.Env[k]))
	}
	return env
}

// quote wraps a value in a Caddyfile string. Caddy's lexer only treats \"
// as an escape and keeps every other backslash as is, so Windows paths go
// in unchanged; a trailing backslash (a drive root) is doubled so it can't
// escape the closing quote.
func quote(s string) string {
	s = strings.ReplaceAll(s, `"`, `\"`)
	if strings.HasSuffix(s, `\`) {
		s += `\`
	}
	return `"` + s + `"`
}
