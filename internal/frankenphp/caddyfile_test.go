package frankenphp

import (
	"strings"
	"testing"
)

func wantAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func wantNone(t *testing.T, out string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(out, n) {
			t.Errorf("unexpected %q in:\n%s", n, out)
		}
	}
}

func TestFrankenCaddyfileClassic(t *testing.T) {
	out := Caddyfile(SiteConf{Name: "blog", Port: 44001, Root: "/p/public"})
	wantAll(t, out, "admin off", "auto_https off", "http://:44001 {\n\tbind 127.0.0.1\n", `root * "/p/public"`, "php_server")
	wantNone(t, out, "frankenphp {", "env ")
	// The address must not carry a host (it would become a Host matcher
	// and reject the forwarded blog.test).
	wantNone(t, out, "http://127.0.0.1")
}

func TestFrankenCaddyfileForwardsHostAndScheme(t *testing.T) {
	secure := Caddyfile(SiteConf{Name: "blog", Port: 44001, Root: "/p/public", Secure: true})
	wantAll(t, secure, "\t\tenv HTTPS on\n", "\t\tenv SERVER_PORT 443\n",
		"servers {\n\t\ttrusted_proxies static 127.0.0.1/32 ::1/128\n\t}")
	plain := Caddyfile(SiteConf{Name: "blog", Port: 44001, Root: "/p/public"})
	wantNone(t, plain, "env HTTPS", "env SERVER_PORT")
	wantAll(t, plain, "trusted_proxies static 127.0.0.1/32 ::1/128")
}

func TestFrankenCaddyfileWatchAndProd(t *testing.T) {
	out := Caddyfile(SiteConf{
		Name: "blog", Port: 44001, Root: "/p/public",
		Prepend: "/m/prepend.php", WatchDir: "/m/watch/blog",
		Env:           map[string]string{"APP_ENV": "production", "APP_DEBUG": "false"},
		FreezeOpcache: true,
	})
	wantAll(t, out,
		"\tfrankenphp {\n\t\tphp_ini auto_prepend_file \"/m/prepend.php\"\n",
		"\t\tphp_ini opcache.validate_timestamps 0\n",
		`env MULLION_WATCH "/m/watch/blog"`,
		"env MULLION_RID {http.request.header.X-Mullion-Rid}",
		`env APP_ENV "production"`,
		`env APP_DEBUG "false"`,
	)
	if strings.Index(out, "env APP_DEBUG") > strings.Index(out, "env APP_ENV") {
		t.Errorf("env keys must be sorted:\n%s", out)
	}
	wantNone(t, out, "env HTTPS")
}

func TestFrankenCaddyfileQuotesPaths(t *testing.T) {
	out := Caddyfile(SiteConf{Port: 1, Root: `C:\Sites\a"b\`, Prepend: `C:\m\p.php`})
	wantAll(t, out, `root * "C:\Sites\a\"b\\"`, `php_ini auto_prepend_file "C:\m\p.php"`)
}
