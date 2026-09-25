package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/gitops"
)

// gitSandbox shuts the developer's global/system git config out of the
// test and returns a helper that runs setup git commands.
func gitSandbox(t *testing.T) func(dir string, args ...string) {
	t.Helper()
	if !gitops.Available() {
		t.Skip("git not installed")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	_ = os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return func(dir string, args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestGitWrappers(t *testing.T) {
	g := gitSandbox(t)
	a := workersSandboxApp(t)
	proj := t.TempDir()
	plain := t.TempDir()
	a.State.AddSite(config.Site{Name: "gitsite-shop", Path: proj})
	a.State.AddSite(config.Site{Name: "gitsite-plain", Path: plain})
	ctx := context.Background()

	if _, err := a.GitStatus(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "no site") {
		t.Errorf("unknown site: %v", err)
	}
	st, err := a.GitStatus(ctx, "gitsite-plain")
	if err != nil || st.IsRepo {
		t.Fatalf("plain folder status = %+v, %v", st, err)
	}

	g(proj, "init", "-q", "-b", "main")
	g(proj, "config", "user.name", "T")
	g(proj, "config", "user.email", "t@example.com")
	_ = os.WriteFile(filepath.Join(proj, "a.txt"), []byte("a\n"), 0o644)
	if err := a.GitStage(ctx, "gitsite-shop", nil, true); err != nil {
		t.Fatal(err)
	}
	hash, err := a.GitCommit(ctx, "gitsite-shop", "first", false, false)
	if err != nil || hash == "" {
		t.Fatalf("commit: %q %v", hash, err)
	}
	_ = os.WriteFile(filepath.Join(proj, "b.txt"), []byte("b\n"), 0o644)
	if err := a.GitStash(ctx, "gitsite-shop", "push", "wip", true, 0); err != nil {
		t.Fatal(err)
	}
	if l, err := a.GitStashList(ctx, "gitsite-shop"); err != nil || len(l) != 1 {
		t.Fatalf("stashes = %v %v", l, err)
	}
	if err := a.GitStash(ctx, "gitsite-shop", "bogus", "", false, 0); err == nil {
		t.Error("unknown stash action accepted")
	}
	if err := a.GitSequencer(ctx, "gitsite-shop", "rm -rf"); err == nil {
		t.Error("unknown sequencer op accepted")
	}
	if l, err := a.GitLog(ctx, "gitsite-shop", 5); err != nil || len(l) != 1 || l[0].Subject != "first" {
		t.Fatalf("log = %v %v", l, err)
	}

	ForgetGitSummary("gitsite-shop")
	ForgetGitSummary("gitsite-plain")
	sums := a.GitSummaries(ctx)
	if s := sums["gitsite-shop"]; s == nil || s.Branch != "main" || s.Changes != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if _, ok := sums["gitsite-plain"]; ok {
		t.Error("a folder outside any repository got a summary")
	}
	// Cached for a few seconds: a new file doesn't show until forgotten.
	_ = os.WriteFile(filepath.Join(proj, "c.txt"), []byte("c\n"), 0o644)
	if s := a.GitSummaries(ctx)["gitsite-shop"]; s.Changes != 0 {
		t.Errorf("cache not used: %+v", s)
	}
	ForgetGitSummary("gitsite-shop")
	if s := a.GitSummaries(ctx)["gitsite-shop"]; s.Changes != 1 {
		t.Errorf("after forget: %+v", s)
	}

	if err := a.GitInit(ctx, "gitsite-plain"); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.GitStatus(ctx, "gitsite-plain"); !st.IsRepo {
		t.Error("init didn't make a repository")
	}
}
