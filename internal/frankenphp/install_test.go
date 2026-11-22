package frankenphp

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want, errHas string
	}{
		{"darwin", "arm64", "frankenphp-mac-arm64", ""},
		{"windows", "amd64", "frankenphp-windows-x86_64.zip", ""},
		{"darwin", "amd64", "", "FrankenPHP has no build for Intel Macs — keep this site on PHP-FPM"},
		{"linux", "amd64", "", "not available"},
		{"windows", "arm64", "", "not available"},
	}
	for _, c := range cases {
		got, err := assetName(c.goos, c.goarch)
		if c.errHas != "" {
			if err == nil || (c.goos == "darwin" && err.Error() != c.errHas) || !strings.Contains(err.Error(), c.errHas) {
				t.Errorf("%s/%s: want error %q, got %q, %v", c.goos, c.goarch, c.errHas, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s/%s: got %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
}

type fakeAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}
type fakeRelease struct {
	Tag        string      `json:"tag_name"`
	Prerelease bool        `json:"prerelease"`
	Draft      bool        `json:"draft"`
	Assets     []fakeAsset `json:"assets"`
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestSkipsReleasesWithoutAsset(t *testing.T) {
	var srvURL string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]fakeRelease{
			{Tag: "v1.15.0-rc1", Prerelease: true, Assets: []fakeAsset{{Name: "frankenphp-mac-arm64", URL: srvURL + "/rc", Digest: "sha256:aa"}}},
			{Tag: "v1.14.1", Draft: true, Assets: []fakeAsset{{Name: "frankenphp-mac-arm64", URL: srvURL + "/draft", Digest: "sha256:bb"}}},
			{Tag: "v1.14.1", Assets: []fakeAsset{{Name: "frankenphp-linux-x86_64", URL: srvURL + "/linux", Digest: "sha256:cc"}}},
			{Tag: "v1.14.0", Assets: []fakeAsset{
				{Name: "frankenphp-linux-x86_64", URL: srvURL + "/linux", Digest: "sha256:cc"},
				{Name: "frankenphp-mac-arm64", URL: srvURL + "/mac", Size: 42, Digest: "sha256:dd"},
			}},
		})
	})
	srvURL = srv.URL
	old := ReleasesAPI
	ReleasesAPI = srv.URL
	t.Cleanup(func() { ReleasesAPI = old })

	rel, err := latest(context.Background(), "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := Release{Tag: "v1.14.0", Asset: Asset{Name: "frankenphp-mac-arm64", URL: srv.URL + "/mac", Size: 42, Digest: "sha256:dd"}}
	if rel != want {
		t.Fatalf("got %+v, want %+v", rel, want)
	}

	if _, err := latest(context.Background(), "windows", "amd64"); err == nil {
		t.Fatal("expected an error when no release has a Windows asset")
	}
}

func TestLatestReportsHTTPErrors(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "rate limited", 403) })
	old := ReleasesAPI
	ReleasesAPI = srv.URL
	t.Cleanup(func() { ReleasesAPI = old })
	if _, err := latest(context.Background(), "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want an HTTP 403 error, got %v", err)
	}
}

func testPaths(t *testing.T) pmdir.Paths {
	t.Helper()
	return pmdir.Paths{Home: filepath.Join(t.TempDir(), "home")}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallVerifiesAndPlacesBinary(t *testing.T) {
	cases := []struct {
		name  string
		asset string
		body  []byte
	}{
		{"raw binary", "frankenphp-mac-arm64", []byte("#!/bin/sh\necho fake\n")},
		{"zip", "frankenphp-windows-x86_64.zip", zipOf(t, map[string]string{
			pmdir.ExeName("frankenphp"): "fake exe",
			"ext/php_curl.dll":          "fake ext",
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(c.body) })
			paths := testPaths(t)
			var msgs []string
			rel := Release{Tag: "v1.14.0", Asset: Asset{Name: c.asset, URL: srv.URL + "/" + c.asset, Size: int64(len(c.body)), Digest: sum(c.body)}}
			if err := Install(context.Background(), paths, rel, func(s string) { msgs = append(msgs, s) }); err != nil {
				t.Fatal(err)
			}
			bin := Binary(paths, "v1.14.0")
			if want := filepath.Join(paths.FrankenDir(), "v1.14.0", pmdir.ExeName("frankenphp")); bin != want {
				t.Fatalf("Binary = %q, want %q", bin, want)
			}
			st, err := os.Stat(bin)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
				t.Fatalf("binary is not executable: %v", st.Mode())
			}
			if len(msgs) == 0 || !strings.HasPrefix(msgs[0], "Downloading FrankenPHP v1.14.0 (") || !strings.HasSuffix(msgs[0], ")…") {
				t.Fatalf("progress = %q", msgs)
			}
			if got := Installed(paths); len(got) != 1 || got[0] != "v1.14.0" {
				t.Fatalf("Installed = %v", got)
			}
			entries, _ := os.ReadDir(paths.FrankenDir())
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".") {
					t.Fatalf("temp folder left behind: %s", e.Name())
				}
			}
			if c.name == "zip" {
				if _, err := os.Stat(filepath.Join(paths.FrankenDir(), "v1.14.0", "ext", "php_curl.dll")); err != nil {
					t.Fatalf("zip siblings not extracted: %v", err)
				}
				if _, err := os.Stat(filepath.Join(paths.FrankenDir(), "v1.14.0", c.asset)); err == nil {
					t.Fatal("the downloaded zip should not stay in the install folder")
				}
			}
		})
	}
}

func TestInstallRejectsBadDigest(t *testing.T) {
	body := []byte("#!/bin/sh\necho fake\n")
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	paths := testPaths(t)
	rel := Release{Tag: "v1.14.0", Asset: Asset{Name: "frankenphp-mac-arm64", URL: srv.URL, Size: int64(len(body)), Digest: sum([]byte("something else"))}}

	err := Install(context.Background(), paths, rel, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want a checksum error, got %v", err)
	}
	entries, _ := os.ReadDir(paths.FrankenDir())
	if len(entries) != 0 {
		t.Fatalf("a failed install left %d entries behind", len(entries))
	}
	if got := Installed(paths); len(got) != 0 {
		t.Fatalf("Installed = %v after a failed install", got)
	}

	rel.Asset.Digest = sum(body)
	if err := Install(context.Background(), paths, rel, nil); err != nil {
		t.Fatalf("retry with the right digest: %v", err)
	}
	if _, err := os.Stat(Binary(paths, "v1.14.0")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRefusesUnverifiableOrUnsafe(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("x")) })
	paths := testPaths(t)
	good := Asset{Name: "frankenphp-mac-arm64", URL: srv.URL, Digest: sum([]byte("x"))}

	noDigest := good
	noDigest.Digest = ""
	if err := Install(context.Background(), paths, Release{Tag: "v1.14.0", Asset: noDigest}, nil); err == nil {
		t.Error("an asset without a digest must not install")
	}
	for _, tag := range []string{"../evil", "v1.14", "1.14.0", ""} {
		if err := Install(context.Background(), paths, Release{Tag: tag, Asset: good}, nil); err == nil {
			t.Errorf("tag %q must be rejected", tag)
		}
	}
}

func TestInstallReplacesExistingTag(t *testing.T) {
	v1, v2 := []byte("one"), []byte("two")
	cur := v1
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(cur) })
	paths := testPaths(t)
	rel := Release{Tag: "v1.14.0", Asset: Asset{Name: "frankenphp-mac-arm64", URL: srv.URL, Digest: sum(v1)}}
	if err := Install(context.Background(), paths, rel, nil); err != nil {
		t.Fatal(err)
	}
	cur, rel.Asset.Digest = v2, sum(v2)
	if err := Install(context.Background(), paths, rel, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Binary(paths, "v1.14.0")); string(b) != "two" {
		t.Fatalf("binary not replaced: %q", b)
	}
}

func TestInstalledNewestFirst(t *testing.T) {
	paths := testPaths(t)
	for _, tag := range []string{"v1.9.0", "v1.14.0", "v1.10.2", "v1.14.0-rc1"} {
		dir := filepath.Join(paths.FrankenDir(), tag)
		os.MkdirAll(dir, 0o755)
		if tag != "v1.14.0-rc1" {
			os.WriteFile(filepath.Join(dir, pmdir.ExeName("frankenphp")), []byte("x"), 0o755)
		}
	}
	os.MkdirAll(filepath.Join(paths.FrankenDir(), ".v1.15.0.tmp"), 0o755)
	os.MkdirAll(filepath.Join(paths.FrankenDir(), "v1.11.0"), 0o755) // no binary: not installed
	got := strings.Join(Installed(paths), ",")
	if got != "v1.14.0,v1.10.2,v1.9.0" {
		t.Fatalf("Installed = %s", got)
	}
	if got := Installed(pmdir.Paths{Home: filepath.Join(t.TempDir(), "nothing")}); len(got) != 0 {
		t.Fatalf("Installed on a missing dir = %v", got)
	}
}

func TestPHPVersionParses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the fake binary")
	}
	script := filepath.Join(t.TempDir(), "frankenphp")
	body := "#!/bin/sh\necho 'FrankenPHP v1.14.0 PHP 8.6.0 Caddy v2.11.9 h1:bhZtcRKQOq0fuLvEgCdPYvUxXyGn9kzjG4lh2332DTE='\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := PHPVersion(script)
	if err != nil || got != "8.6.0" {
		t.Fatalf("PHPVersion = %q, %v; want 8.6.0", got, err)
	}

	bad := filepath.Join(t.TempDir(), "frankenphp")
	os.WriteFile(bad, []byte("#!/bin/sh\necho hello\n"), 0o755)
	if _, err := PHPVersion(bad); err == nil {
		t.Fatal("expected an error when the output has no PHP version")
	}
	if _, err := PHPVersion(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}

func TestPHPVersionIgnoresInheritedPHPRC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the fake binary")
	}
	// A script that prints the version only when PHPRC is unset, like
	// the real binary prints warnings (and may fail) under a foreign ini.
	script := filepath.Join(t.TempDir(), "frankenphp")
	os.WriteFile(script, []byte("#!/bin/sh\n[ -z \"$PHPRC\" ] || exit 3\necho 'FrankenPHP v1.14.0 PHP 8.6.0 Caddy v2.11.9'\n"), 0o755)
	t.Setenv("PHPRC", "/somewhere/else")
	if got, err := PHPVersion(script); err != nil || got != "8.6.0" {
		t.Fatalf("PHPVersion = %q, %v", got, err)
	}
}
