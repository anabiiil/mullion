package caddy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGenerateProdMode(t *testing.T) {
	off := Generate([]SiteConf{{Host: "blog.test", Root: "/p/blog/public", FcgiPort: 9084}}, "/h/logs")
	if strings.Contains(off, "@hashed") || strings.Contains(off, "APP_ENV") || strings.Contains(off, "validate_timestamps") {
		t.Fatalf("prod off must render nothing extra:\n%s", off)
	}
	on := Generate([]SiteConf{
		{Host: "blog.test", Root: "/p/blog/public", FcgiPort: 9084, ValidateTimestamps: "0",
			Prod: &ProdConf{Env: map[string]string{"APP_ENV": "production", "APP_DEBUG": "false"}}},
		{Host: "shop.test", Root: "/p/shop/public", FcgiPort: 9084, ValidateTimestamps: "1"},
	}, "/h/logs")
	blog := on[:strings.Index(on, "http://shop.test")]
	for _, want := range []string{
		"\t@hashed path_regexp ",
		"\theader @hashed Cache-Control \"public, max-age=31536000, immutable\"\n",
		"\t\tenv PHP_VALUE \"opcache.validate_timestamps=0\"\n",
		"\t\tenv APP_DEBUG \"false\"\n\t\tenv APP_ENV \"production\"\n", // sorted
	} {
		if !strings.Contains(blog, want) {
			t.Fatalf("prod site missing %q:\n%s", want, blog)
		}
	}
	shop := on[strings.Index(on, "http://shop.test"):]
	if !strings.Contains(shop, "env PHP_VALUE \"opcache.validate_timestamps=1\"") || strings.Contains(shop, "APP_ENV") || strings.Contains(shop, "@hashed") {
		t.Fatalf("the other site on the same php-fpm pins its own value only:\n%s", shop)
	}
	// Static/node build sites: cache headers, no PHP.
	st := Generate([]SiteConf{{Host: "app.test", Kind: "static", Root: "/p/dist", Prod: &ProdConf{}}}, "/h/logs")
	if !strings.Contains(st, "@hashed") || strings.Contains(st, "php_fastcgi") {
		t.Fatalf("static prod:\n%s", st)
	}
}

// Watch + profiler + prod mode compose: one PHP_VALUE (newline-joined),
// the profiler's own env lines untouched.
func TestGenerateWatchProfilerProd(t *testing.T) {
	out := Generate([]SiteConf{{Host: "blog.test", Root: "/p", FcgiPort: 9084, ValidateTimestamps: "0",
		Watch:      &WatchConf{Prepend: "/w/mullion-watch.php", Dir: "/w/blog", RequestID: true},
		Prod:       &ProdConf{Env: map[string]string{"APP_ENV": "production"}},
		FastCGIEnv: [][2]string{{"PHP_ADMIN_VALUE", "spx.http_enabled=1\nspx.http_key=k"}, {"MULLION_SPX_CHAIN", "/w/mullion-watch.php"}},
	}}, "/h/logs")
	if n := strings.Count(out, "env PHP_VALUE"); n != 1 {
		t.Fatalf("want one PHP_VALUE line, got %d:\n%s", n, out)
	}
	for _, want := range []string{
		"\t\tenv PHP_VALUE \"auto_prepend_file=\\\"/w/mullion-watch.php\\\"\nopcache.validate_timestamps=0\"\n",
		"\t\tenv MULLION_WATCH \"/w/blog\"\n",
		"\t\tenv APP_ENV \"production\"\n",
		"\t\tenv PHP_ADMIN_VALUE \"spx.http_enabled=1\nspx.http_key=k\"\n",
		"\t\tenv MULLION_SPX_CHAIN \"/w/mullion-watch.php\"\n",
		"\tlog_append mullion_rid {http.request.uuid}\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	// Profiler alone still opens the block.
	p := Generate([]SiteConf{{Host: "a.test", Root: "/p", FcgiPort: 9084, FastCGIEnv: [][2]string{{"PHP_ADMIN_VALUE", "x=1"}}}}, "/h/logs")
	if !strings.Contains(p, "php_fastcgi 127.0.0.1:9084 {\n\t\tenv PHP_ADMIN_VALUE \"x=1\"\n\t}\n") {
		t.Fatalf("profiler only:\n%s", p)
	}
	// A real Caddy, when one is around, accepts the result.
	if caddy := os.Getenv("MULLION_TEST_CADDY"); caddy != "" {
		f := filepath.Join(t.TempDir(), "Caddyfile")
		os.WriteFile(f, []byte(out), 0o644)
		if b, err := exec.Command(caddy, "adapt", "--config", f).CombinedOutput(); err != nil {
			t.Fatalf("caddy adapt: %v\n%s", err, b)
		}
	}
}

func TestHashedAssetsRegexp(t *testing.T) {
	re := regexp.MustCompile(hashedAssets)
	for _, p := range []string{"/build/assets/app-BkL3x9Qa.js", "/assets/index-3f2a9c1b.css", "/_next/static/chunks/main-abcdef1234.js", "/static/js/main.3f2a9c1b.js"} {
		if !re.MatchString(p) {
			t.Errorf("%s should be cached", p)
		}
	}
	for _, p := range []string{"/css/app.css", "/js/bootstrap-something.js", "/build/manifest.json", "/index.php", "/assets/logo.png"} {
		if re.MatchString(p) {
			t.Errorf("%s must not be cached", p)
		}
	}
}
