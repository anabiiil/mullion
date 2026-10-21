package caddy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	sites := []SiteConf{
		{Host: "blog.test", Root: `C:\code\blog\public`, FcgiPort: 9083, Secure: false},
		{Host: "shop.test", Root: `C:\code\shop`, FcgiPort: 9074, Secure: true},
	}
	out := Generate(sites, `C:\Users\x\.mullion\logs`)

	// Plain-HTTP site must carry the http:// prefix so Caddy doesn't
	// auto-redirect it to HTTPS.
	if !strings.Contains(out, "http://blog.test {") {
		t.Fatalf("insecure site should be addressed as http://:\n%s", out)
	}
	if strings.Contains(out, "http://shop.test") {
		t.Fatalf("secure site must not be forced to http:\n%s", out)
	}
	if !strings.Contains(out, "shop.test {") || !strings.Contains(out, "tls internal") {
		t.Fatalf("secure site must use tls internal:\n%s", out)
	}
	if !strings.Contains(out, "php_fastcgi 127.0.0.1:9083") ||
		!strings.Contains(out, "php_fastcgi 127.0.0.1:9074") {
		t.Fatalf("per-site fastcgi ports missing:\n%s", out)
	}
	// Windows paths are quoted with their backslashes kept single: Caddy's
	// lexer only unescapes \", so doubling them would reach Caddy doubled.
	if !strings.Contains(out, `"C:\code\blog\public"`) {
		t.Fatalf("root path not quoted:\n%s", out)
	}
	if got := quote(`C:\`); got != `"C:\\"` {
		t.Fatalf("a trailing backslash must not escape the closing quote: %s", got)
	}
	// Caddy parses skip_install_trust as a bare flag (its argument is
	// ignored), so emitting `skip_install_trust false` would disable
	// trust-store installation and break the browser padlock.
	if strings.Contains(out, "skip_install_trust") {
		t.Fatalf("skip_install_trust must not be emitted:\n%s", out)
	}
}

func TestGenerateEmpty(t *testing.T) {
	out := Generate(nil, `C:\logs`)
	if !strings.Contains(out, "admin 127.0.0.1:2019") {
		t.Fatalf("global admin block missing:\n%s", out)
	}
	// A finite grace period, so a reload never waits forever on a
	// long-lived streaming request through one of the sites.
	if !strings.Contains(out, "\tgrace_period 10s\n") {
		t.Fatalf("global grace_period missing:\n%s", out)
	}
}

func TestGenerateKinds(t *testing.T) {
	sites := []SiteConf{
		{Host: "app.test", Kind: "node", ProxyPort: 5173},
		{Host: "vapp.test", Kind: "node", ProxyPort: 5199, RewriteHost: true},
		{Name: "down", Host: "down.test", Kind: "node", ProxyPort: 0, AgentPort: 42999},
		{Name: "dead", Host: "dead.test", Kind: "node", ProxyPort: 0},
		{Host: "app-build.test", Kind: "static", Root: "/x/dist", Secure: true},
		{Host: "blog.test", Root: "/y", FcgiPort: 9084},
	}
	out := Generate(sites, "/logs")
	for _, want := range []string{
		"reverse_proxy localhost:5173",
		"reverse_proxy localhost:5199 {",
		"header_up Host {upstream_hostport}",
		"keepalive off",
		"max_conns_per_host 64",
		"respond ",
		"reverse_proxy 127.0.0.1:42999",
		`header_up X-Mullion-Wake "down"`,
		`root * "/x/dist"`,
		"try_files {path} /index.html",
		"php_fastcgi 127.0.0.1:9084",
		"tls internal",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("generated Caddyfile misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "reverse_proxy localhost:0") {
		t.Fatalf("down site must not proxy to port 0:\n%s", out)
	}
}

func TestGenerateAliases(t *testing.T) {
	sites := []SiteConf{
		{Name: "shop", Host: "shop.test", Aliases: []string{"api.shop.test", "*.shop.test"}, Root: "/s", FcgiPort: 9084},
		{Name: "app", Host: "app.test", Aliases: []string{"*.app.test"}, Kind: "node", ProxyPort: 5173, RewriteHost: true, Secure: true},
		{Name: "plain", Host: "plain.test", Root: "/p", FcgiPort: 9084},
	}
	out := Generate(sites, "/logs")
	for _, want := range []string{
		"\nhttp://shop.test, http://api.shop.test, http://*.shop.test {\n",
		"\napp.test, *.app.test {\n\ttls internal\n",
		"\nhttp://plain.test {\n",
		// One log per site, named after the primary host.
		// Caddyfile strings keep backslashes as is (Windows paths).
		`output file "` + filepath.Join("/logs", "shop.test.log") + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("generated Caddyfile misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "api.shop.test.log") || strings.Contains(out, "*.shop.test.log") {
		t.Fatalf("aliases must not get their own log files:\n%s", out)
	}
}

// A "proxy" block (Mail's inbox) reverse-proxies to the local service
// over https with the internal CA, streaming responses.
func TestGenerateProxySite(t *testing.T) {
	out := Generate([]SiteConf{{Name: "mail", Host: "mail.test", Kind: "proxy", ProxyPort: 8025, Secure: true}}, "/logs")
	want := "\nmail.test {\n\ttls internal\n\treverse_proxy 127.0.0.1:8025 {\n\t\tflush_interval -1\n\t}\n\tlog {\n"
	if !strings.Contains(out, want) {
		t.Errorf("proxy block:\n%s", out)
	}
	if strings.Contains(out, "php_fastcgi") || strings.Contains(out, "file_server") {
		t.Errorf("proxy block must not serve files:\n%s", out)
	}
}

func TestGenerateWatchPHPValue(t *testing.T) {
	off := Generate([]SiteConf{{Host: "blog.test", Root: "/p/blog/public", FcgiPort: 9084}}, "/h/logs")
	if !strings.Contains(off, "\tphp_fastcgi 127.0.0.1:9084\n") || strings.Contains(off, "PHP_VALUE") ||
		strings.Contains(off, "MULLION_WATCH") || strings.Contains(off, "log_append") {
		t.Fatalf("watch off must render the plain php_fastcgi line:\n%s", off)
	}

	on := Generate([]SiteConf{
		{Host: "blog.test", Root: "/p/blog/public", FcgiPort: 9084, Watch: &WatchConf{
			Prepend: "/Users/me/.mullion/watch/mullion-watch.php", Dir: "/Users/me/.mullion/watch/blog", RequestID: true}},
		{Host: "shop.test", Root: "/p/shop/public", FcgiPort: 9084},
	}, "/h/logs")
	want := "\tphp_fastcgi 127.0.0.1:9084 {\n" +
		"\t\tenv PHP_VALUE \"auto_prepend_file=\\\"/Users/me/.mullion/watch/mullion-watch.php\\\"\"\n" +
		"\t\tenv MULLION_WATCH \"/Users/me/.mullion/watch/blog\"\n" +
		"\t\tenv MULLION_RID {http.request.uuid}\n" +
		"\t}\n" +
		"\tlog_append mullion_rid {http.request.uuid}\n"
	if !strings.Contains(on, want) {
		t.Fatalf("watched site block missing:\n%s", on)
	}
	// Only the watched site gets it.
	shop := on[strings.Index(on, "http://shop.test"):]
	if strings.Contains(shop, "PHP_VALUE") || !strings.Contains(shop, "\tphp_fastcgi 127.0.0.1:9084\n") {
		t.Fatalf("an unwatched site must not load the prepend:\n%s", shop)
	}

	// No request IDs (Caddy older than 2.8): no log_append, no MULLION_RID.
	old := Generate([]SiteConf{{Host: "blog.test", Root: "/p", FcgiPort: 9084, Watch: &WatchConf{Prepend: "/w.php", Dir: "/w"}}}, "/h/logs")
	if strings.Contains(old, "log_append") || strings.Contains(old, "MULLION_RID") || !strings.Contains(old, "env PHP_VALUE") {
		t.Fatalf("without request IDs:\n%s", old)
	}
	// Node sites ignore Watch.
	node := Generate([]SiteConf{{Host: "app.test", Kind: "node", ProxyPort: 5173, Watch: &WatchConf{Prepend: "/w.php"}}}, "/h/logs")
	if strings.Contains(node, "PHP_VALUE") {
		t.Fatalf("node site:\n%s", node)
	}
}

func TestVersionAtLeast(t *testing.T) {
	for out, want := range map[string]bool{
		"v2.11.4 h1:XKxk": true, "v2.8.0": true, "v2.7.6 h1:x": false, "v3.0.0": true, "garbage": false,
	} {
		if got := versionAtLeast(out, 2, 8); got != want {
			t.Errorf("%q: got %v", out, got)
		}
	}
}
