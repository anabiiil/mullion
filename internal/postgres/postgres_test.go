package postgres

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// A trimmed copy of https://www.postgresql.org/versions.json.
const versionsFixture = `[
 {"current": false, "eolDate": "2021-11-11", "latestMinor": "24", "major": "9.6", "supported": false},
 {"current": false, "eolDate": "2025-11-13", "latestMinor": "23", "major": "13", "supported": false},
 {"current": false, "eolDate": "2026-11-12", "latestMinor": "24", "major": "14", "supported": true},
 {"current": false, "eolDate": "2027-11-11", "latestMinor": "19", "major": "15", "supported": true},
 {"current": false, "eolDate": "2028-11-09", "latestMinor": "15", "major": "16", "supported": true},
 {"current": false, "eolDate": "2029-11-08", "latestMinor": "11", "major": "17", "supported": true},
 {"current": true,  "eolDate": "2030-11-14", "latestMinor": "6",  "major": "18", "supported": true}
]`

func stubReleases(t *testing.T, fail bool, missing ...string) {
	t.Helper()
	oldFetch, oldAvail := fetchReleases, available
	t.Cleanup(func() { fetchReleases, available = oldFetch, oldAvail })
	fetchReleases = func(ctx context.Context) ([]release, error) {
		if fail {
			return nil, errors.New("offline")
		}
		return parseReleases([]byte(versionsFixture))
	}
	available = func(ctx context.Context, v string) (bool, error) {
		for _, m := range missing {
			if m == v {
				return false, nil
			}
		}
		return true, nil
	}
}

func TestParseReleases(t *testing.T) {
	rels, err := parseReleases([]byte(versionsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 6 || rels[0].Major != 18 || rels[0].Latest != 6 || rels[len(rels)-1].Major != 13 {
		t.Fatalf("unexpected releases: %+v", rels)
	}
	if rels[len(rels)-1].Supported {
		t.Fatal("13 should be unsupported")
	}
	if _, err := parseReleases([]byte(`not json`)); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestResolveVersion(t *testing.T) {
	if platformSupported() != nil {
		t.Skip("no PostgreSQL builds for this platform")
	}
	ctx := context.Background()
	stubReleases(t, false, "16.15")
	cases := map[string]string{
		"":        "17.11",
		"latest":  "18.6",
		"LATEST ": "18.6",
		"16":      "16.14", // EDB lagging: steps back a minor
		"13":      "13.23", // unsupported but still resolvable
		"17.6":    "17.6",
	}
	for arg, want := range cases {
		got, err := ResolveVersion(ctx, arg)
		if err != nil || got != want {
			t.Errorf("ResolveVersion(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "17.x", "9.6", "17.6.1", "99"} {
		if got, err := ResolveVersion(ctx, bad); err == nil {
			t.Errorf("ResolveVersion(%q) = %q, want an error", bad, got)
		}
	}

	stubReleases(t, false, "17.11", "17.10", "17.9")
	if got, err := ResolveVersion(ctx, "17"); err == nil {
		t.Errorf("expected no build found, got %q", got)
	}

	stubReleases(t, true)
	if got, err := ResolveVersion(ctx, ""); err != nil || got != pinnedLatest[DefaultSeries] {
		t.Errorf("offline fallback = %q, %v", got, err)
	}
}

func TestAvailableSeries(t *testing.T) {
	stubReleases(t, false)
	got, _ := AvailableSeries(context.Background())
	if strings.Join(got, ",") != "18,17,16,15,14" {
		t.Errorf("AvailableSeries = %v", got)
	}
	stubReleases(t, true)
	got, _ = AvailableSeries(context.Background())
	if len(got) != len(pinnedLatest) || got[0] != "18" {
		t.Errorf("offline AvailableSeries = %v", got)
	}
}

func TestVersionHelpers(t *testing.T) {
	if Label("17.6") != "PostgreSQL 17.6" {
		t.Error(Label("17.6"))
	}
	if Major("17.6") != "17" || Major("17") != "17" {
		t.Error("Major")
	}
	if compare("17.10", "17.9") <= 0 || compare("9.6.24", "10.1") >= 0 || compare("16", "16.0") != 0 {
		t.Error("compare")
	}
	paths := pmdir.Paths{Home: "/h"}
	if DataDir(paths, "17.6") != DataDir(paths, "17.11") || DataDir(paths, "16.2") == DataDir(paths, "17.6") {
		t.Error("data dirs must be per major")
	}
	if filepath.Base(DataDir(paths, "17.6")) != "data-17" {
		t.Error(DataDir(paths, "17.6"))
	}
	if urls := downloadURLs("17.6"); edbPlatform != "" &&
		(len(urls) == 0 || !strings.HasSuffix(urls[len(urls)-1], "postgresql-17.6-1-"+edbPlatform+"-binaries.zip")) {
		t.Errorf("downloadURLs = %v", urls)
	}
}

func TestDatabaseNames(t *testing.T) {
	for _, ok := range []string{"app", "My_App2", strings.Repeat("a", 63)} {
		if checkName(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "my-app", "a b", `x"; DROP`, "é", strings.Repeat("a", 64)} {
		if checkName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	paths := pmdir.Paths{Home: t.TempDir()}
	for _, sys := range []string{"postgres", "template0", "Template1"} {
		if err := DropDatabase(paths, "17.6", sys); err == nil || !strings.Contains(err.Error(), "system database") {
			t.Errorf("DropDatabase(%q) = %v", sys, err)
		}
	}
	if err := CreateDatabase(paths, "17.6", "bad-name"); err == nil {
		t.Error("CreateDatabase accepted an invalid name")
	}
	if got := dropSQL("17.6", "app"); got != `DROP DATABASE "app" WITH (FORCE);` {
		t.Error(got)
	}
	if got := dropSQL("12.20", "app"); !strings.Contains(got, "pg_terminate_backend") || !strings.HasSuffix(got, `DROP DATABASE "app";`) {
		t.Error(got)
	}
	if quoteLiteral("it's") != "'it''s'" {
		t.Error(quoteLiteral("it's"))
	}
}

func TestRenderConfig(t *testing.T) {
	conf := renderConf("darwin")
	for _, want := range []string{"listen_addresses = '127.0.0.1'", "port = 5432", "unix_socket_directories = '/tmp'"} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(renderConf("windows"), "unix_socket") {
		t.Error("windows conf must not set a socket dir")
	}

	trust := renderHba("darwin", false)
	if !strings.Contains(trust, "127.0.0.1/32  trust") || !strings.Contains(trust, "::1/128       trust") ||
		!strings.Contains(trust, "local   all") || strings.Contains(trust, "scram") {
		t.Errorf("trust hba:\n%s", trust)
	}
	scram := renderHba("windows", true)
	if strings.Contains(scram, "trust") || strings.Contains(scram, "local") || !strings.Contains(scram, "127.0.0.1/32  scram-sha-256") {
		t.Errorf("scram hba:\n%s", scram)
	}
	if strings.Contains(trust+scram, "0.0.0.0") {
		t.Error("hba must never open non-local addresses")
	}
}

func TestWriteConfIdempotent(t *testing.T) {
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "postgresql.conf"), []byte("port = 1234"), 0o600)
	for i := 0; i < 2; i++ {
		if err := writeConf(data); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := os.ReadFile(filepath.Join(data, "postgresql.conf"))
	if n := strings.Count(string(body), "include_if_exists = 'mullion.conf'"); n != 1 {
		t.Errorf("include appears %d times:\n%s", n, body)
	}
	if _, err := os.Stat(filepath.Join(data, "mullion.conf")); err != nil {
		t.Error(err)
	}
}

func TestWantEntry(t *testing.T) {
	keep := []string{"pgsql/", "pgsql/bin/postgres", "pgsql/bin/psql.exe", "pgsql/lib/libpq.5.dylib",
		"pgsql/share/postgresql.conf.sample", "pgsql/include/libpq-fe.h", "pgsql/server_license.txt"}
	drop := []string{"pgsql/pgAdmin 4.app/Contents/Info.plist", "pgsql/pgAdmin 4/runtime/pgAdmin4.exe",
		"pgsql/stackbuilder.app/x", "pgsql/StackBuilder/share/x", "pgsql/doc/postgresql/html/index.html",
		"pgsql/bin/stackbuilder.exe", "pgsql/bin/wxbase3211u_vc_x64_custom.dll", "pgsql/pgAdmin_license.txt"}
	for _, n := range keep {
		if !wantEntry(n) {
			t.Errorf("should keep %s", n)
		}
	}
	for _, n := range drop {
		if wantEntry(n) {
			t.Errorf("should drop %s", n)
		}
	}
}

func TestPlanSpans(t *testing.T) {
	entries := []cdEntry{
		{"pgsql/bin/a", 0}, {"pgsql/bin/b", 100},
		{"pgsql/pgAdmin 4/x", 200}, {"pgsql/pgAdmin 4/y", 10_000},
		{"pgsql/lib/c", 50_000}, {"pgsql/doc/d", 60_000}, {"pgsql/share/e", 60_050},
	}
	spans := planSpans(entries, 70_000, wantEntry, 1000)
	want := []span{{off: 0, end: 200}, {off: 50_000, end: 70_000}}
	if len(spans) != len(want) {
		t.Fatalf("spans = %+v", spans)
	}
	for i := range want {
		if spans[i].off != want[i].off || spans[i].end != want[i].end {
			t.Fatalf("spans = %+v, want %+v", spans, want)
		}
	}
}

// buildZip makes an EDB-shaped archive with a large pgAdmin entry in
// the middle, so a selective fetch has to skip it.
func buildZip(t *testing.T) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, body []byte, mode os.FileMode, method uint16) {
		h := &zip.FileHeader{Name: name, Method: method}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	add("pgsql/", nil, os.ModeDir|0o755, zip.Store)
	add("pgsql/bin/postgres", []byte("#!/bin/sh\necho postgres\n"), 0o755, zip.Deflate)
	add("pgsql/bin/psql", []byte("psql"), 0o755, zip.Deflate)
	noise := make([]byte, 6<<20)
	rand.New(rand.NewSource(1)).Read(noise)
	add("pgsql/pgAdmin 4/big.bin", noise, 0o644, zip.Store)
	add("pgsql/lib/libpq.dylib", bytes.Repeat([]byte("lib"), 1000), 0o755, zip.Deflate)
	add("pgsql/lib/libpq.5.dylib", []byte("libpq.dylib"), os.ModeSymlink|0o777, zip.Store)
	add("pgsql/share/extension/plpgsql.control", []byte("control"), 0o644, zip.Deflate)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchZip(t *testing.T) {
	old := tailBytes
	tailBytes = 64 << 10
	t.Cleanup(func() { tailBytes = old })
	data := buildZip(t)
	for _, ranges := range []bool{true, false} {
		var requests, served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if !ranges {
				r.Header.Del("Range")
			}
			cw := &countingWriter{ResponseWriter: w, n: &served}
			http.ServeContent(cw, r, "pg.zip", time.Time{}, bytes.NewReader(data))
		}))
		dest := filepath.Join(t.TempDir(), "out")
		if err := fetchZip(context.Background(), srv.URL+"/pg.zip", dest, t.TempDir(), wantEntry); err != nil {
			t.Fatalf("ranges=%v: %v", ranges, err)
		}
		srv.Close()
		body, err := os.ReadFile(filepath.Join(dest, "pgsql", "bin", "postgres"))
		if err != nil || !strings.Contains(string(body), "echo postgres") {
			t.Fatalf("ranges=%v: postgres = %q, %v", ranges, body, err)
		}
		if runtime.GOOS != "windows" {
			if info, _ := os.Stat(filepath.Join(dest, "pgsql", "bin", "postgres")); info.Mode().Perm()&0o100 == 0 {
				t.Errorf("ranges=%v: exec bit lost: %v", ranges, info.Mode())
			}
			if link, err := os.Readlink(filepath.Join(dest, "pgsql", "lib", "libpq.5.dylib")); err != nil || link != "libpq.dylib" {
				t.Errorf("ranges=%v: symlink = %q, %v", ranges, link, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dest, "pgsql", "share", "extension", "plpgsql.control")); err != nil {
			t.Errorf("ranges=%v: %v", ranges, err)
		}
		if _, err := os.Stat(filepath.Join(dest, "pgsql", "pgAdmin 4")); err == nil {
			t.Errorf("ranges=%v: pgAdmin was extracted", ranges)
		}
		if ranges {
			if served.Load() > int64(len(data))/2 {
				t.Errorf("selective fetch downloaded %d of %d bytes", served.Load(), len(data))
			}
			if requests.Load() > 4 {
				t.Errorf("selective fetch used %d requests", requests.Load())
			}
		}
	}
}

func TestFetchZipMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "AccessDenied", http.StatusForbidden)
	}))
	defer srv.Close()
	err := fetchZip(context.Background(), srv.URL+"/x.zip", t.TempDir(), t.TempDir(), wantEntry)
	if err == nil || !strings.Contains(err.Error(), "HTTP 4") {
		t.Fatalf("want an HTTP 4xx error, got %v", err)
	}
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.ResponseWriter.Write(p)
}
