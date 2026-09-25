package mongodb

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

// testdata/full.json is a trimmed copy of downloads.mongodb.org/full.json
// (same field names), with an RC, an alpha, flagged releases, an
// enterprise-only entry per version, and old series lacking builds.
func loadFixture(t *testing.T, target, arch string) []release {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rels, err := parseIndex(f, target, arch)
	if err != nil {
		t.Fatal(err)
	}
	return rels
}

func TestParseIndexFiltersToStableBaseBuilds(t *testing.T) {
	rels := loadFixture(t, "macos", "arm64")
	var got []string
	for _, r := range rels {
		got = append(got, r.Version)
		if strings.Contains(r.URL, "enterprise") || !strings.HasPrefix(r.URL, "https://fastdl.mongodb.org/osx/mongodb-macos-arm64-") {
			t.Errorf("%s: unexpected URL %s", r.Version, r.URL)
		}
		if len(r.SHA256) != 64 {
			t.Errorf("%s: missing sha256", r.Version)
		}
	}
	want := []string{"8.3.11", "8.2.12", "8.2.9", "8.0.32", "8.0.23", "7.0.43", "6.0.29"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, r := range rels {
		if flagged := r.Version == "8.2.9" || r.Version == "8.0.23"; r.Warning != flagged {
			t.Errorf("%s: Warning = %v", r.Version, r.Warning)
		}
	}
}

func TestSelectVersion(t *testing.T) {
	rels := loadFixture(t, "macos", "arm64")
	cases := map[string]string{
		"":       "8.0.32",
		"latest": "8.3.11",
		"LATEST": "8.3.11",
		"8.0":    "8.0.32",
		"v8.0":   "8.0.32",
		"7.0":    "7.0.43",
		"8.2":    "8.2.12",
		"8.0.32": "8.0.32",
		"8.0.23": "8.0.23", // flagged, but asked for by name
	}
	for arg, want := range cases {
		got, err := selectVersion(rels, arg)
		if err != nil || got != want {
			t.Errorf("selectVersion(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	for _, arg := range []string{"5.0", "5.0.34", "9.0.0-rc0", "8", "foo", "8.0.999"} {
		if got, err := selectVersion(rels, arg); err == nil {
			t.Errorf("selectVersion(%q) = %q, want an error", arg, got)
		}
	}
}

func TestSelectVersionSkipsFlaggedNewest(t *testing.T) {
	rels := []release{
		{Version: "8.0.23", Warning: true},
		{Version: "8.0.22"},
		{Version: "8.0.21", Warning: true},
	}
	if got, err := selectVersion(rels, "8.0"); err != nil || got != "8.0.22" {
		t.Fatalf("got %q, %v; want 8.0.22", got, err)
	}
	if _, err := selectVersion([]release{{Version: "8.0.23", Warning: true}}, ""); err == nil {
		t.Fatal("a series with only flagged releases must not resolve")
	}
}

func TestSeriesListPerPlatform(t *testing.T) {
	mac := seriesList(loadFixture(t, "macos", "arm64"))
	if want := []string{"8.3", "8.2", "8.0", "7.0", "6.0"}; !reflect.DeepEqual(mac, want) {
		t.Errorf("macos: got %v, want %v", mac, want)
	}
	// 5.0 has Windows builds but is below minSeries.
	win := seriesList(loadFixture(t, "windows", "x86_64"))
	if want := []string{"8.3", "8.2", "8.0", "7.0", "6.0"}; !reflect.DeepEqual(win, want) {
		t.Errorf("windows: got %v, want %v", win, want)
	}
	for _, r := range loadFixture(t, "windows", "x86_64") {
		if !strings.HasSuffix(r.URL, "/mongodb-windows-x86_64-"+r.Version+".zip") {
			t.Errorf("windows %s: unexpected URL %s", r.Version, r.URL)
		}
	}
}

func TestParseMongoshIndex(t *testing.T) {
	for _, tc := range []struct{ distro, arch, suffix string }{
		{"darwin", "arm64", "-darwin-arm64.zip"},
		{"darwin", "x86_64", "-darwin-x64.zip"},
		{"win32", "x86_64", "-win32-x64.zip"},
	} {
		f, err := os.Open(filepath.Join("testdata", "mongosh.json"))
		if err != nil {
			t.Fatal(err)
		}
		rel, err := parseMongoshIndex(f, tc.distro, tc.arch)
		f.Close()
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.distro, tc.arch, err)
		}
		// The fixture's first entry is a beta; it must be skipped.
		if rel.Version != "2.12.0" || !strings.HasSuffix(rel.URL, "mongosh-2.12.0"+tc.suffix) || len(rel.SHA256) != 64 {
			t.Errorf("%s/%s: got %+v", tc.distro, tc.arch, rel)
		}
	}
}

func TestNameValidation(t *testing.T) {
	for _, ok := range []string{"app", "my_app-2", "A", strings.Repeat("x", 63)} {
		if err := checkName(ok); err != nil {
			t.Errorf("checkName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a.b", "a b", "a/b", `a"b`, "a$b", "é", strings.Repeat("x", 64), "x');db.dropDatabase('"} {
		if err := checkName(bad); err == nil {
			t.Errorf("checkName(%q) accepted", bad)
		}
	}
	paths := pmdir.Paths{Home: t.TempDir()}
	for _, sys := range []string{"admin", "local", "config", "Admin"} {
		if err := DropDatabase(paths, sys); err == nil || !strings.Contains(err.Error(), "system database") {
			t.Errorf("DropDatabase(%q) = %v, want a refusal", sys, err)
		}
	}
}

func TestRenderConf(t *testing.T) {
	home := filepath.Join(t.TempDir(), "it's home")
	paths := pmdir.Paths{Home: home}
	conf := renderConf(paths)
	for _, want := range []string{
		"  bindIp: 127.0.0.1\n",
		"  port: 27017\n",
		"  dbPath: '" + strings.ReplaceAll(filepath.Join(home, "mongodb", "data"), "'", "''") + "'\n",
		"  destination: file\n",
		"  path: '" + strings.ReplaceAll(filepath.Join(home, "logs", "mongodb.log"), "'", "''") + "'\n",
		"  logAppend: true\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("mongod.conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "security") || strings.Contains(conf, "authorization") {
		t.Errorf("local dev config must not enable auth:\n%s", conf)
	}
	if got := yamlQuote(`C:\Mullion\mongodb\data`); got != `'C:\Mullion\mongodb\data'` {
		t.Errorf("yamlQuote kept Windows path as %s", got)
	}
}

func TestEnsureInitializedAndLayout(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	if DataInitialized(paths) {
		t.Fatal("fresh home reported initialized")
	}
	if err := EnsureInitialized(paths, "8.0.32"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "mongodb", "mongod.conf")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(DataDir(paths)); err != nil || !info.IsDir() {
		t.Fatalf("data dir: %v", err)
	}
	// Fake installs: only dirs with bin/mongod count; mongosh picks newest.
	mk := func(rel string) {
		p := filepath.Join(paths.Home, "mongodb", rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o755)
	}
	mk(filepath.Join("7.0.43", "bin", pmdir.ExeName("mongod")))
	mk(filepath.Join("8.0.32", "bin", pmdir.ExeName("mongod")))
	mk(filepath.Join("8.0.9", "bin", pmdir.ExeName("mongod")))
	mk(filepath.Join("8.1.0", "README"))
	mk(filepath.Join("mongosh-2.9.0", "bin", pmdir.ExeName("mongosh")))
	mk(filepath.Join("mongosh-2.12.0", "bin", pmdir.ExeName("mongosh")))
	if got, want := Installed(paths), []string{"8.0.32", "8.0.9", "7.0.43"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Installed = %v, want %v", got, want)
	}
	if got := MongoshPath(paths); !strings.Contains(got, "mongosh-2.12.0") {
		t.Errorf("MongoshPath = %q", got)
	}
}

func TestParseNameList(t *testing.T) {
	got, err := parseNameList("some warning\n[\"admin\",\"app\"]\n")
	if err != nil || !reflect.DeepEqual(got, []string{"admin", "app"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parseNameList("MongoServerError: nope"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLabelAndURI(t *testing.T) {
	if Label("8.0.12") != "MongoDB 8.0.12" || ConnectionURI() != "mongodb://127.0.0.1:27017" {
		t.Fatal("unexpected Label/ConnectionURI")
	}
}

func TestLastLogErrorReportsRootCauseOfLatestRun(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	os.MkdirAll(paths.LogsDir(), 0o755)
	log := strings.Join([]string{
		`{"s":"I","msg":"Build Info"}`,
		`{"s":"E","msg":"old run failure"}`,
		`{"s":"I","msg":"Build Info"}`,
		`not json`,
		`{"s":"E","c":"WT","msg":"WiredTiger error message","attr":{"error":13,"message":{"msg":"open: Permission denied"}}}`,
		`{"s":"F","msg":"Fatal assertion","attr":{"msgid":50853}}`,
		`{"s":"E","msg":"Error setting up listener","attr":{"error":{"codeName":"SocketException"}}}`,
	}, "\n")
	os.WriteFile(logFile(paths), []byte(log), 0o644)
	if got := lastLogError(paths); got != ": open: Permission denied" {
		t.Fatalf("got %q", got)
	}
}
