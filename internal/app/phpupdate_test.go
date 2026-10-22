package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/junction"
	"pm/internal/phpver"
	"pm/internal/pmdir"
)

// fakePHPRelease stubs the network and the real installer: Resolve
// answers latest, Install creates a version dir with a generated
// php.ini (fail makes it error instead), and Verify passes unless
// verifyErr is set. Updates reports latest for every installed branch.
type fakePHPRelease struct {
	latest     string
	installErr error
	verifyErr  error
	installs   []string
}

func (f *fakePHPRelease) install(t *testing.T) {
	t.Helper()
	oldR, oldI, oldV, oldU := phpResolve, phpInstall, phpVerify, phpUpdates
	t.Cleanup(func() { phpResolve, phpInstall, phpVerify, phpUpdates = oldR, oldI, oldV, oldU })
	phpResolve = func(ctx context.Context, sel phpver.Selector) (phpver.Release, error) {
		return phpver.Release{Version: f.latest}, nil
	}
	phpInstall = func(ctx context.Context, paths pmdir.Paths, rel phpver.Release) (string, error) {
		f.installs = append(f.installs, rel.Version)
		dir := paths.PhpVersionDir(rel.Version)
		if err := os.MkdirAll(filepath.Join(dir, "ext"), 0o755); err != nil {
			return "", err
		}
		if f.installErr != nil {
			return "", f.installErr // a half-done install leaves its dir behind
		}
		if _, err := os.Stat(filepath.Join(dir, "php.ini")); err == nil {
			return dir, nil // already installed: no-op, like the real one
		}
		return dir, os.WriteFile(filepath.Join(dir, "php.ini"), []byte("; generated\nmemory_limit=512M\n"), 0o644)
	}
	phpVerify = func(paths pmdir.Paths, version string) error { return f.verifyErr }
	phpUpdates = func(ctx context.Context, paths pmdir.Paths, installed []string, force bool) (map[string]phpver.Update, error) {
		out := map[string]phpver.Update{}
		for _, v := range installed {
			if phpver.Branch(v) == phpver.Branch(f.latest) && phpver.Compare(v, f.latest) < 0 {
				out[v] = phpver.Update{From: v, To: f.latest, Security: true}
			}
		}
		return out, nil
	}
}

// phpUpdateSandbox: 8.4.23 global (with a customised php.ini) and
// 8.3.1, sites pinned to each, one following the global version.
func phpUpdateSandbox(t *testing.T) *App {
	t.Helper()
	a := workersSandboxApp(t)
	for _, v := range []string{"8.4.23", "8.3.1"} {
		dir := a.Paths.PhpVersionDir(v)
		ini := "memory_limit=1G\nopcache.jit=tracing\n" +
			`curl.cainfo="` + filepath.Join(dir, "cacert.pem") + `"` + "\n"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "php.ini"), []byte(ini), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.UseGlobal("8.4.23"); err != nil {
		t.Fatal(err)
	}
	a.State.Sites = []config.Site{
		{Name: "legacy", Path: "/p/legacy", PHP: "8.3.1"},
		{Name: "follows", Path: "/p/follows"},
		{Name: "pinned", Path: "/p/pinned", PHP: "8.4.23", Workers: []config.Worker{{ID: "q", Name: "Queue", Command: "php artisan queue:work"}}},
		{Name: "front", Path: "/p/front", Kind: "node"},
	}
	a.State.Config.MailCatchPHP = true
	a.State.Config.MailIniPrev = map[string]map[string]string{"8.4.23": {"sendmail_path": `"/usr/sbin/sendmail -t -i"`}}
	if err := a.State.Save(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPlanPHPUpdate(t *testing.T) {
	a := phpUpdateSandbox(t)
	p := a.PlanPHPUpdate("8.4.23", "8.4.27")
	if !p.Global || !slices.Equal(p.Sites, []string{"pinned"}) || p.Following != 1 || p.Workers != 1 || p.TargetInstalled {
		t.Errorf("plan for the global version = %+v", p)
	}
	p = a.PlanPHPUpdate("8.3.1", "8.3.9")
	if p.Global || !slices.Equal(p.Sites, []string{"legacy"}) || p.Following != 0 || p.Workers != 0 {
		t.Errorf("plan for a pinned version = %+v", p)
	}
}

func TestUpdatePHPSwitchesEverything(t *testing.T) {
	a := phpUpdateSandbox(t)
	f := &fakePHPRelease{latest: "8.4.27"}
	f.install(t)
	var log []string
	res, err := a.UpdatePHP(context.Background(), "8.4", PHPUpdateOptions{Log: func(s string) { log = append(log, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "8.4.23" || res.To != "8.4.27" || !res.Removed || !res.Security || !res.Global {
		t.Errorf("result = %+v", res)
	}
	c := a.State.Config
	if c.GlobalPHP != "8.4.27" {
		t.Errorf("GlobalPHP = %q", c.GlobalPHP)
	}
	if got := junction.Target(a.Paths.CurrentPhp()); filepath.Clean(got) != filepath.Clean(a.Paths.PhpVersionDir("8.4.27")) {
		t.Errorf("current -> %q, want the new version dir", got)
	}
	if s := a.State.FindSite("pinned"); s.PHP != "8.4.27" {
		t.Errorf("pinned site still on %q", s.PHP)
	}
	if s := a.State.FindSite("legacy"); s.PHP != "8.3.1" {
		t.Errorf("another branch's site was moved: %q", s.PHP)
	}
	if s := a.State.FindSite("follows"); s.PHP != "" {
		t.Errorf("a following site got pinned: %q", s.PHP)
	}
	if _, ok := c.MailIniPrev["8.4.23"]; ok || c.MailIniPrev["8.4.27"]["sendmail_path"] == "" {
		t.Errorf("Mail's remembered php.ini values did not move: %+v", c.MailIniPrev)
	}
	if _, err := os.Stat(a.Paths.PhpVersionDir("8.4.23")); !os.IsNotExist(err) {
		t.Errorf("old version dir still there (err=%v)", err)
	}
	ini := readTestFile(t, filepath.Join(a.Paths.PhpVersionDir("8.4.27"), "php.ini"))
	if !strings.Contains(ini, "memory_limit=1G") || !strings.Contains(ini, filepath.Join(a.Paths.PhpVersionDir("8.4.27"), "cacert.pem")) {
		t.Errorf("php.ini not carried over / re-pointed:\n%s", ini)
	}
	// Persisted, not just in memory.
	st, err := config.Load(a.Paths)
	if err != nil || st.Config.GlobalPHP != "8.4.27" || st.FindSite("pinned").PHP != "8.4.27" {
		t.Errorf("saved state: global %q, pinned %q (%v)", st.Config.GlobalPHP, st.FindSite("pinned").PHP, err)
	}
	if len(log) == 0 {
		t.Error("no progress lines logged")
	}
}

func TestUpdatePHPKeepOld(t *testing.T) {
	a := phpUpdateSandbox(t)
	(&fakePHPRelease{latest: "8.3.9"}).install(t)
	res, err := a.UpdatePHP(context.Background(), "8.3.1", PHPUpdateOptions{KeepOld: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed || res.Global {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(a.Paths.PhpVersionDir("8.3.1")); err != nil {
		t.Errorf("--keep-old removed the old version: %v", err)
	}
	if a.State.Config.GlobalPHP != "8.4.23" || a.State.FindSite("legacy").PHP != "8.3.9" {
		t.Errorf("global %q / legacy %q", a.State.Config.GlobalPHP, a.State.FindSite("legacy").PHP)
	}
}

// assertUntouched checks a failed update left 8.4.23 exactly as it was.
func assertUntouched(t *testing.T, a *App, iniBefore string) {
	t.Helper()
	if a.State.Config.GlobalPHP != "8.4.23" || a.State.FindSite("pinned").PHP != "8.4.23" {
		t.Errorf("state switched anyway: global %q, pinned %q", a.State.Config.GlobalPHP, a.State.FindSite("pinned").PHP)
	}
	st, _ := config.Load(a.Paths)
	if st.Config.GlobalPHP != "8.4.23" || st.FindSite("pinned").PHP != "8.4.23" {
		t.Errorf("saved state switched anyway")
	}
	if got := readTestFile(t, filepath.Join(a.Paths.PhpVersionDir("8.4.23"), "php.ini")); got != iniBefore {
		t.Errorf("old php.ini changed")
	}
	if got := junction.Target(a.Paths.CurrentPhp()); filepath.Clean(got) != filepath.Clean(a.Paths.PhpVersionDir("8.4.23")) {
		t.Errorf("current -> %q", got)
	}
	if _, err := os.Stat(a.Paths.PhpVersionDir("8.4.27")); !os.IsNotExist(err) {
		t.Errorf("the failed new install was left behind (err=%v)", err)
	}
}

func TestUpdatePHPInstallFailureLeavesOldIntact(t *testing.T) {
	a := phpUpdateSandbox(t)
	before := readTestFile(t, filepath.Join(a.Paths.PhpVersionDir("8.4.23"), "php.ini"))
	(&fakePHPRelease{latest: "8.4.27", installErr: errors.New("download failed")}).install(t)
	if _, err := a.UpdatePHP(context.Background(), "8.4.23", PHPUpdateOptions{}); err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, a, before)
}

func TestUpdatePHPVerifyFailureLeavesOldIntact(t *testing.T) {
	a := phpUpdateSandbox(t)
	before := readTestFile(t, filepath.Join(a.Paths.PhpVersionDir("8.4.23"), "php.ini"))
	(&fakePHPRelease{latest: "8.4.27", verifyErr: errors.New("Unable to load dynamic library ldap.so")}).install(t)
	if _, err := a.UpdatePHP(context.Background(), "8.4", PHPUpdateOptions{}); err == nil || !strings.Contains(err.Error(), "not switching") {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, a, before)
}

func TestUpdatePHPUpToDate(t *testing.T) {
	a := phpUpdateSandbox(t)
	f := &fakePHPRelease{latest: "8.4.23"}
	f.install(t)
	if _, err := a.UpdatePHP(context.Background(), "8.4", PHPUpdateOptions{}); !errors.Is(err, ErrPHPUpToDate) {
		t.Fatalf("err = %v, want ErrPHPUpToDate", err)
	}
	if len(f.installs) != 0 {
		t.Errorf("installed %v for an up-to-date version", f.installs)
	}
	if _, err := a.UpdatePHP(context.Background(), "8", PHPUpdateOptions{}); err == nil {
		t.Error("a major-only selector was accepted")
	}
	if _, err := a.UpdatePHP(context.Background(), "7.4", PHPUpdateOptions{}); err == nil {
		t.Error("a branch that isn't installed was accepted")
	}
}

// TestUpdatePHPTargetAlreadyInstalled: 8.4.27 is already there (with its
// own php.ini) — it is switched to, but its php.ini is not replaced.
func TestUpdatePHPTargetAlreadyInstalled(t *testing.T) {
	a := phpUpdateSandbox(t)
	dir := a.Paths.PhpVersionDir("8.4.27")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "php.ini"), []byte("memory_limit=2G\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	(&fakePHPRelease{latest: "8.4.27"}).install(t)
	res, err := a.UpdatePHP(context.Background(), "8.4.23", PHPUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TargetInstalled || len(res.Warnings) == 0 || a.State.Config.GlobalPHP != "8.4.27" {
		t.Errorf("result = %+v", res)
	}
	if got := readTestFile(t, filepath.Join(dir, "php.ini")); got != "memory_limit=2G\n" {
		t.Errorf("the existing php.ini was replaced: %q", got)
	}
}

func TestUpdateAllPHP(t *testing.T) {
	a := phpUpdateSandbox(t)
	(&fakePHPRelease{latest: "8.4.27"}).install(t)
	res, err := a.UpdateAllPHP(context.Background(), PHPUpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].From != "8.4.23" {
		t.Fatalf("results = %+v", res)
	}
	got, _ := phpver.Installed(a.Paths)
	if !slices.Equal(got, []string{"8.4.27", "8.3.1"}) {
		t.Errorf("installed = %v", got)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
