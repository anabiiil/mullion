package caddy

import (
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
	// Windows paths must survive quoting with escaped backslashes.
	if !strings.Contains(out, `"C:\\code\\blog\\public"`) {
		t.Fatalf("root path not quoted/escaped:\n%s", out)
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
}

func TestGenerateKinds(t *testing.T) {
	sites := []SiteConf{
		{Host: "app.test", Kind: "node", ProxyPort: 5173},
		{Host: "down.test", Kind: "node", ProxyPort: 0},
		{Host: "app-build.test", Kind: "static", Root: "/x/dist", Secure: true},
		{Host: "blog.test", Root: "/y", FcgiPort: 9084},
	}
	out := Generate(sites, "/logs")
	for _, want := range []string{
		"reverse_proxy localhost:5173",
		"respond ",
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
