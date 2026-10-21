package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain makes sure no test can ever reach the real Homebrew, Scoop or
// GitHub: every test that needs one points these at fakes itself.
func TestMain(m *testing.M) {
	brewCandidates = nil
	brewPrefixes = nil
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	scoopRoots = func() []string { return nil }
	APIBase = "http://127.0.0.1:1" // nothing listens there
	binaryVersion = func(context.Context, string) (string, error) {
		return "", errors.New("binaryVersion not stubbed in this test")
	}
	directTarget = func() (string, error) { return "", errors.New("directTarget not stubbed in this test") }
	os.Exit(m.Run())
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.2", "2.1.1", true},
		{"2.1.1", "2.1.1", false},
		{"2.1.1", "2.1.2", false},
		{"2.1.10", "2.1.9", true},
		{"v3.0.0", "2.9.9", true},
		{"2.2", "2.1.9", true},
		{"2.1", "2.1.0", false},
		{"2.2.0", "2.2.0-rc.1", true},
		{"2.2.0-rc.1", "2.2.0", false},
		{"2.2.0-rc.2", "2.2.0-rc.1", true},
		{"2.2.0-rc.1", "2.1.9", true},
		{"2.1.1+build5", "2.1.1", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestChannelFor(t *testing.T) {
	cases := []struct {
		exe         string
		brew, scoop bool
		want        Channel
		desc        string
	}{
		{"/opt/homebrew/Cellar/mullion/2.1.1/bin/mullion", false, false, Homebrew, "Apple-silicon Homebrew"},
		{"/usr/local/Cellar/mullion/2.1.1/bin/mullion", false, false, Homebrew, "Intel Homebrew"},
		{"/home/linuxbrew/.linuxbrew/Cellar/mullion/2.1.1/bin/mullion", false, false, Homebrew, "Linuxbrew"},
		{`C:\Users\dev\scoop\apps\mullion\current\mullion.exe`, false, false, Scoop, "Scoop current"},
		{`C:\Users\dev\scoop\apps\mullion\2.1.1\mullion.exe`, false, false, Scoop, "Scoop version dir (junction resolved)"},
		{`C:\ProgramData\scoop\apps\mullion\current\mullion.exe`, false, false, Scoop, "Scoop global"},
		{`D:\Tools\Scoop\Apps\Mullion\current\mullion.exe`, false, false, Scoop, "Scoop, odd casing"},
		{"/Users/dev/.mullion/bin/mullion", false, false, Direct, "install.sh copy"},
		{`C:\Users\dev\.mullion\bin\mullion.exe`, false, false, Direct, "downloaded exe's installed copy"},
		{`C:\Users\dev\Downloads\mullion.exe`, false, false, Direct, "downloaded exe"},
		{`C:\Users\dev\.mullion\bin\mullion.exe`, false, true, Scoop, "Scoop user running the bin copy"},
		{"/Users/dev/.mullion/bin/mullion", true, false, Homebrew, "brew user running an old copy"},
		{"/opt/homebrew/Cellar/mullion-other/1.0/bin/mullion", false, false, Direct, "another formula"},
	}
	for _, c := range cases {
		if got := channelFor(c.exe, c.brew, c.scoop); got != c.want {
			t.Errorf("%s: channelFor(%q) = %q, want %q", c.desc, c.exe, got, c.want)
		}
	}
}

func TestChannelLabelsAndManual(t *testing.T) {
	if Homebrew.Label() != "Homebrew" || Scoop.Label() != "Scoop" || Direct.Label() != "Direct download" {
		t.Fatal("unexpected channel labels")
	}
	if got := Homebrew.ManualCommand(); got != "brew update && brew upgrade anabiiil/tap/mullion" {
		t.Errorf("brew manual = %q", got)
	}
	if got := Scoop.ManualCommand(); !strings.Contains(got, "scoop update mullion") {
		t.Errorf("scoop manual = %q", got)
	}
}

func TestBrewInstalledMullion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew detection is off on Windows")
	}
	dir := t.TempDir()
	brewPrefixes = []string{filepath.Join(dir, "none"), dir}
	defer func() { brewPrefixes = nil }()
	if _, ok := brewInstalledMullion(); ok {
		t.Fatal("found mullion in an empty prefix")
	}
	os.MkdirAll(filepath.Join(dir, "Cellar", "mullion", "2.1.1"), 0o755)
	if got, ok := brewInstalledMullion(); !ok || got != filepath.Join(dir, "Cellar", "mullion") {
		t.Fatalf("brewInstalledMullion = %q, %v", got, ok)
	}
}

func TestPickAsset(t *testing.T) {
	all := []Asset{
		{Name: "mullion-v2.2.0-darwin-amd64.tar.gz"},
		{Name: "mullion-v2.2.0-darwin-arm64.tar.gz"},
		{Name: "mullion-v2.2.0-windows-amd64.zip"},
		{Name: "mullion.exe"},
	}
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "mullion-v2.2.0-darwin-arm64.tar.gz"},
		{"darwin", "amd64", "mullion-v2.2.0-darwin-amd64.tar.gz"},
		{"windows", "amd64", "mullion.exe"},
		{"windows", "arm64", "mullion.exe"},
		{"linux", "amd64", ""},
		{"darwin", "386", ""},
	}
	for _, c := range cases {
		a, ok := pickAsset(all, c.goos, c.goarch)
		if (c.want == "") == ok || a.Name != c.want {
			t.Errorf("pickAsset(%s/%s) = %q, %v; want %q", c.goos, c.goarch, a.Name, ok, c.want)
		}
	}
	// No raw exe published: the zip is next best.
	if a, ok := pickAsset(all[:3], "windows", "amd64"); !ok || a.Name != "mullion-v2.2.0-windows-amd64.zip" {
		t.Errorf("zip fallback = %q, %v", a.Name, ok)
	}
}

func TestFindChecksum(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	manifest := "0000  other.tar.gz\n" + strings.Repeat("cd", 32) + "  other.zip\n" + sum + " *mullion.exe\n"
	if got := findChecksum(manifest, "mullion.exe", false); got != sum {
		t.Errorf("manifest lookup = %q", got)
	}
	if got := findChecksum(sum+"\n", "mullion.exe", true); got != sum {
		t.Errorf("single-file lookup = %q", got)
	}
	if got := findChecksum(manifest, "missing", false); got != "" {
		t.Errorf("missing = %q", got)
	}
}

func TestParseVersionOutput(t *testing.T) {
	if v, err := parseVersionOutput("mullion version 2.2.0\n"); err != nil || v != "2.2.0" {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := parseVersionOutput("garbage"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestSwapExeWindows(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mullion.exe")
	os.WriteFile(target, []byte("v1"), 0o755)
	newFile := filepath.Join(dir, ".mullion-new.exe")
	os.WriteFile(newFile, []byte("v2"), 0o755)

	if err := swapExe(target, newFile, "windows"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "v2")
	assertFile(t, filepath.Join(dir, "mullion.old.exe"), "v1")
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Fatal("the new file was not moved into place")
	}

	// Next upgrade: a parked copy that can't be deleted (still running —
	// simulated by a non-empty directory, which os.Remove also refuses)
	// must not block the swap.
	os.Remove(filepath.Join(dir, "mullion.old.exe"))
	os.MkdirAll(filepath.Join(dir, "mullion.old.exe", "busy"), 0o755)
	os.WriteFile(newFile, []byte("v3"), 0o755)
	if err := swapExe(target, newFile, "windows"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "v3")
	parked, _ := filepath.Glob(filepath.Join(dir, "mullion.old-*.exe"))
	if len(parked) != 1 {
		t.Fatalf("parked copies = %v", parked)
	}
	assertFile(t, parked[0], "v2")

	// CleanupOld removes the parked files it can.
	cleanupOldIn(dir)
	if left, _ := filepath.Glob(filepath.Join(dir, "mullion.old-*.exe")); len(left) != 0 {
		t.Fatalf("cleanup left %v", left)
	}
	if _, err := os.Stat(filepath.Join(dir, "mullion.old.exe")); err != nil {
		t.Fatal("cleanup removed a non-empty directory")
	}
	assertFile(t, target, "v3")
}

func TestSwapExeWindowsRollsBack(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mullion.exe")
	os.WriteFile(target, []byte("v1"), 0o755)
	// newFile missing: the second rename fails and v1 must come back.
	if err := swapExe(target, filepath.Join(dir, "missing.exe"), "windows"); err == nil {
		t.Fatal("swap with a missing new file succeeded")
	}
	assertFile(t, target, "v1")
}

func TestSwapExeUnix(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "mullion")
	os.WriteFile(target, []byte("v1"), 0o755)
	newFile := filepath.Join(dir, ".mullion-new")
	os.WriteFile(newFile, []byte("v2"), 0o600)
	if err := swapExe(target, newFile, "darwin"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "v2")
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(target); info.Mode().Perm()&0o111 == 0 {
			t.Fatal("the new binary is not executable")
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*old*")); len(m) != 0 {
		t.Fatalf("unix swap parked %v", m)
	}
}

func TestOldExeName(t *testing.T) {
	if got := oldExeName(filepath.Join("bin", "mullion.exe")); got != filepath.Join("bin", "mullion.old.exe") {
		t.Errorf("got %q", got)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

// ── Latest and its cache ──────────────────────────────────────────

func releaseServer(t *testing.T, rel Release, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/anabiiil/mullion/releases/latest" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		json.NewEncoder(w).Encode(rel)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withAPI(t *testing.T, base string) {
	t.Helper()
	old := APIBase
	APIBase = base
	resetCache()
	t.Cleanup(func() { APIBase = old; resetCache(); now = time.Now })
}

func TestLatestCache(t *testing.T) {
	var hits atomic.Int32
	srv := releaseServer(t, Release{Tag: "v9.0.0", URL: "https://example/rel"}, &hits)
	withAPI(t, srv.URL)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rel, err := Latest(ctx)
		if err != nil || rel.Version() != "9.0.0" {
			t.Fatalf("Latest = %+v, %v", rel, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("3 lookups within the TTL hit GitHub %d times", hits.Load())
	}
	clock = clock.Add(29 * time.Minute)
	Latest(ctx)
	if hits.Load() != 1 {
		t.Fatal("cache expired early")
	}
	clock = clock.Add(2 * time.Minute)
	Latest(ctx)
	if hits.Load() != 2 {
		t.Fatalf("cache did not expire after 31 minutes (hits=%d)", hits.Load())
	}
	LatestFresh(ctx)
	if hits.Load() != 3 {
		t.Fatal("LatestFresh answered from the cache")
	}

	info := Check(ctx, false)
	if hits.Load() != 3 || info.Latest != "9.0.0" || !info.Update || info.URL != "https://example/rel" {
		t.Fatalf("Check = %+v (hits %d)", info, hits.Load())
	}
	Check(ctx, true)
	if hits.Load() != 4 {
		t.Fatal("Check(force) answered from the cache")
	}
}

func TestLatestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	withAPI(t, srv.URL)
	if _, err := Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "rate-limiting") {
		t.Fatalf("403 error = %v", err)
	}
	// Failures are not cached.
	info := Check(context.Background(), false)
	if info.Error == "" || info.Update || info.Latest != "" {
		t.Fatalf("Check on error = %+v", info)
	}
}

func TestExcerpt(t *testing.T) {
	if excerpt("short", 100) != "short" {
		t.Fatal("short notes changed")
	}
	long := strings.Repeat("line of notes\n", 100)
	got := excerpt(long, 100)
	if len(got) > 104 || !strings.HasSuffix(got, "…") {
		t.Fatalf("excerpt = %q", got)
	}
}

// ── Homebrew and Scoop with fake executables ─────────────────────

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeBrewWithFakeBrew(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake brew")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	brew := filepath.Join(dir, "brew")
	writeScript(t, brew, `echo "$@" >> "`+calls+`"
echo "==> fake brew $1"
echo "PATH has brew dir: $(echo "$PATH" | grep -c "`+dir+`")"
echo "no color: $HOMEBREW_NO_COLOR" >&2
`)
	brewCandidates = []string{filepath.Join(dir, "missing-brew"), brew}
	geteuid = func() int { return 501 }
	defer func() { brewCandidates = nil; geteuid = os.Geteuid }()

	var lines []string
	exe, err := upgradeBrew(context.Background(), Reporter{Line: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if exe != filepath.Join(dir, "mullion") {
		t.Errorf("new exe = %q", exe)
	}
	data, _ := os.ReadFile(calls)
	if got := string(data); got != "update\nupgrade anabiiil/tap/mullion\n" {
		t.Errorf("brew calls = %q", got)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"==> fake brew update", "==> fake brew upgrade", "PATH has brew dir: 1", "no color: 1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("streamed output lacks %q:\n%s", want, joined)
		}
	}
}

func TestUpgradeBrewFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake brew")
	}
	// Not installed.
	_, err := upgradeBrew(context.Background(), Reporter{})
	var ue *Error
	if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "brew upgrade anabiiil/tap/mullion") {
		t.Fatalf("missing brew error = %v", err)
	}

	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	writeScript(t, brew, `if [ "$1" = upgrade ]; then echo "Error: permission denied" >&2; exit 1; fi
if [ "$1" = update ]; then exit 1; fi
`)
	brewCandidates = []string{brew}
	defer func() { brewCandidates = nil; geteuid = os.Geteuid }()

	// Root is refused before brew runs.
	geteuid = func() int { return 0 }
	if _, err := upgradeBrew(context.Background(), Reporter{}); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("root error = %v", err)
	}
	geteuid = func() int { return 501 }
	var lines []string
	_, err = upgradeBrew(context.Background(), Reporter{Line: func(s string) { lines = append(lines, s) }})
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "brew upgrade failed") {
		t.Fatalf("upgrade failure = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "brew update failed") || !strings.Contains(joined, "permission denied") {
		t.Fatalf("output = %s", joined)
	}
}

func TestBrewEnv(t *testing.T) {
	env := brewEnv([]string{"PATH=/x", "HOME=/h", "HOMEBREW_NO_INSTALL_CLEANUP=1"}, "/opt/homebrew/bin")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=/opt/homebrew/bin:/x:/usr/bin:/bin") || strings.Contains(joined, "NO_INSTALL_CLEANUP") || !strings.Contains(joined, "HOME=/h") {
		t.Fatalf("env = %v", env)
	}
}

func TestScoopCommand(t *testing.T) {
	name, args := scoopCommand(`C:\Users\o'neil\scoop\shims\scoop.ps1`)
	if name != "powershell" || args[len(args)-2] != "-Command" {
		t.Fatalf("command = %s %v", name, args)
	}
	script := args[len(args)-1]
	for _, want := range []string{
		`$scoop = 'C:\Users\o''neil\scoop\shims\scoop.ps1'`,
		"& $scoop update\n",
		"& $scoop update mullion",
		"config ignore_running_processes $true",
		"finally",
		"config rm ignore_running_processes",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "& $scoop update\n") > strings.Index(script, "& $scoop update mullion") {
		t.Error("scoop update must run before scoop update mullion")
	}
}

func TestFindScoopAndInstalled(t *testing.T) {
	root := t.TempDir()
	scoopRoots = func() []string { return []string{filepath.Join(root, "nope"), root} }
	defer func() { scoopRoots = func() []string { return nil } }()
	if _, ok := findScoop(); ok {
		t.Fatal("found scoop in an empty root")
	}
	os.MkdirAll(filepath.Join(root, "shims"), 0o755)
	os.WriteFile(filepath.Join(root, "shims", "scoop.ps1"), nil, 0o644)
	if p, ok := findScoop(); !ok || p != filepath.Join(root, "shims", "scoop.ps1") {
		t.Fatalf("findScoop = %q, %v", p, ok)
	}
	os.MkdirAll(filepath.Join(root, "apps", "mullion", "current"), 0o755)
	os.WriteFile(filepath.Join(root, "apps", "mullion", "current", "mullion.exe"), nil, 0o755)
	p, ok := scoopInstalledMullion()
	if runtime.GOOS == "windows" {
		if !ok || p != filepath.Join(root, "apps", "mullion", "current", "mullion.exe") {
			t.Fatalf("scoopInstalledMullion = %q, %v", p, ok)
		}
	} else if ok {
		t.Fatal("Scoop detection must be off outside Windows")
	}
}

func TestRunStreamContextCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow")
	writeScript(t, script, "echo started\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	var got []string
	err := runStream(ctx, script, nil, nil, func(s string) { got = append(got, s) })
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("runStream did not stop on cancel: %v after %s", err, time.Since(start))
	}
	if len(got) == 0 || got[0] != "started" {
		t.Fatalf("lines = %v", got)
	}
}

// ── direct download, end to end against httptest ─────────────────

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestUpgradeDirect(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the fixture serves a darwin archive")
	}
	archive := tarGz(t, "mullion", []byte("NEW BINARY"))
	sum := sha256.Sum256(archive)
	assetName := fmt.Sprintf("mullion-v9.0.0-darwin-%s.tar.gz", runtime.GOARCH)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	digest := "sha256:" + hex.EncodeToString(sum[:])
	mux.HandleFunc("/dl/"+assetName, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })

	dir := t.TempDir()
	target := filepath.Join(dir, "mullion")
	os.WriteFile(target, []byte("OLD BINARY"), 0o755)
	directTarget = func() (string, error) { return target, nil }
	var checked string
	binaryVersion = func(_ context.Context, exe string) (string, error) {
		checked = exe
		data, _ := os.ReadFile(exe)
		if string(data) != "NEW BINARY" {
			return "", errors.New("wrong binary")
		}
		return "9.0.0", nil
	}
	defer func() {
		directTarget = func() (string, error) { return "", errors.New("unstubbed") }
		binaryVersion = func(context.Context, string) (string, error) { return "", errors.New("unstubbed") }
	}()

	rel := Release{Tag: "v9.0.0", Assets: []Asset{{Name: assetName, URL: srv.URL + "/dl/" + assetName, Size: int64(len(archive)), Digest: digest}}}

	// A bad digest is refused and the old binary stays.
	bad := rel
	bad.Assets = []Asset{rel.Assets[0]}
	bad.Assets[0].Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := upgradeDirect(context.Background(), bad, Reporter{}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	assertFile(t, target, "OLD BINARY")

	var lines []string
	exe, err := upgradeDirect(context.Background(), rel, Reporter{Line: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if exe != target {
		t.Errorf("exe = %q", exe)
	}
	assertFile(t, target, "NEW BINARY")
	if !strings.HasPrefix(filepath.Base(checked), ".mullion-new-") {
		t.Errorf("version check ran on %q, not the staged file", checked)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "sha256 verified") {
		t.Errorf("log = %v", lines)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".mullion-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// ── process discovery ────────────────────────────────────────────

func TestClassify(t *testing.T) {
	cases := []struct {
		exe  string
		args []string
		want Role
	}{
		{"/opt/homebrew/Cellar/mullion/2.1.1/bin/mullion", []string{"agent"}, RoleAgent},
		{"/Users/x/.mullion/bin/mullion", []string{"ui", "--detached"}, RolePanel},
		{"/Users/x/.mullion/bin/mullion", []string{"ui", "--window-host"}, RolePanelHost},
		{"/Applications/Mullion.app/Contents/MacOS/Mullion", nil, RoleMacApp},
		{"/Applications/Mullion Terminal.app/Contents/MacOS/Mullion Terminal", nil, RoleNone},
		{`C:\Users\x\.mullion\bin\mullion.exe`, []string{"tray"}, RoleTray},
		{`C:\Users\x\.mullion\bin\mullion.old.exe`, []string{"tray"}, RoleTray},
		{`C:\Users\x\.mullion\bin\mullion.old-17.exe`, []string{"agent"}, RoleAgent},
		{"/Users/x/.mullion/bin/mullion", []string{"upgrade"}, RoleNone},
		{"/Users/x/.mullion/bin/mullion", nil, RoleNone},
		{"/usr/bin/caddy", []string{"run"}, RoleNone},
	}
	for _, c := range cases {
		if got := classify(c.exe, c.args); got != c.want {
			t.Errorf("classify(%q, %v) = %q, want %q", c.exe, c.args, got, c.want)
		}
	}
}

func TestParsePS(t *testing.T) {
	comm := `  1 /sbin/launchd
 200 /Applications/Mullion.app/Contents/MacOS/Mullion
 201 /opt/homebrew/Cellar/mullion/2.1.1/bin/mullion
 300 /opt/homebrew/Cellar/mullion/2.1.1/bin/mullion
 400 /Applications/Mullion Terminal.app/Contents/MacOS/Mullion Terminal
 500 /Users/x/.mullion/bin/mullion
 600 /Users/x/.mullion/bin/mullion
`
	args := `  1 /sbin/launchd
 200 /Applications/Mullion.app/Contents/MacOS/Mullion
 201 /opt/homebrew/bin/mullion ui --window-host
 300 /opt/homebrew/Cellar/mullion/2.1.1/bin/mullion agent
 400 /Applications/Mullion Terminal.app/Contents/MacOS/Mullion Terminal --flag
 500 /Users/x/.mullion/bin/mullion ui --detached
 600 /Users/x/.mullion/bin/mullion upgrade --finish
`
	got := parsePS(comm, args, 600)
	want := map[int]Role{200: RoleMacApp, 201: RolePanelHost, 300: RoleAgent, 500: RolePanel}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, p := range got {
		if want[p.PID] != p.Role {
			t.Errorf("pid %d role %q, want %q", p.PID, p.Role, want[p.PID])
		}
	}
}

func TestParseWinProcs(t *testing.T) {
	out := "10\tC:\\Users\\x\\.mullion\\bin\\mullion.exe\t\"C:\\Users\\x\\.mullion\\bin\\mullion.exe\" tray\r\n" +
		"11\tC:\\Users\\x\\.mullion\\bin\\mullion.old.exe\tC:\\Users\\x\\.mullion\\bin\\mullion.exe agent\r\n" +
		"12\tC:\\Users\\x\\scoop\\apps\\mullion\\current\\mullion.exe\t\"C:\\Users\\x\\scoop\\apps\\mullion\\current\\mullion.exe\" ui\r\n" +
		"13\t\t\"C:\\Program Files\\x\\mullion.exe\" ui --detached\r\n" +
		"14\tC:\\x\\mullion.exe\tmullion.exe upgrade --finish\r\n" +
		"99\tC:\\x\\mullion.exe\t\"C:\\x\\mullion.exe\" tray\r\n" +
		"garbage\r\n"
	got := parseWinProcs(out, 99)
	want := map[int]Role{10: RoleTray, 11: RoleAgent, 12: RolePanel, 13: RolePanel}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, p := range got {
		if want[p.PID] != p.Role {
			t.Errorf("pid %d role %q, want %q", p.PID, p.Role, want[p.PID])
		}
	}
}

func TestWinArgs(t *testing.T) {
	cases := map[string][]string{
		`"C:\Program Files\m\mullion.exe" ui --detached`: {`C:\Program Files\m\mullion.exe`, "ui", "--detached"},
		`C:\m\mullion.exe tray`:                          {`C:\m\mullion.exe`, "tray"},
		`"C:\m\mullion.exe"`:                             {`C:\m\mullion.exe`},
		``:                                               nil,
	}
	for in, want := range cases {
		if got := winArgs(in); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("winArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
