package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Every test repo lives in t.TempDir(); the user's global/system git
// config is shut out (GIT_CONFIG_GLOBAL points at a scratch file) so
// signing, hooks or aliases on the developer's machine can't leak in.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "gitops-test-")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(tmp, "gitconfig")
	_ = os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n[advice]\n\tdetachedHead = false\n"), 0o644)
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func needGit(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("git not installed")
	}
}

// g runs setup git commands in a test repo.
func g(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_EDITOR=true", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a repository with repo-local identity and one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	needGit(t)
	dir := t.TempDir()
	g(t, dir, "init", "-q", "-b", "main")
	g(t, dir, "config", "user.name", "Test User")
	g(t, dir, "config", "user.email", "test@example.com")
	writeFile(t, dir, "README.md", "hello\nworld\n")
	g(t, dir, "add", "README.md")
	g(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// withRemote gives dir a bare "origin" and pushes main to it.
func withRemote(t *testing.T, dir string) string {
	t.Helper()
	bare := t.TempDir()
	g(t, bare, "init", "-q", "--bare", "-b", "main")
	g(t, dir, "remote", "add", "origin", bare)
	g(t, dir, "push", "-q", "-u", "origin", "main")
	return bare
}

// clone makes a second working copy of bare (with identity).
func clone(t *testing.T, bare string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "other")
	g(t, filepath.Dir(dir), "clone", "-q", bare, dir)
	g(t, dir, "config", "user.name", "Other User")
	g(t, dir, "config", "user.email", "other@example.com")
	return dir
}

func paths(l []FileChange) string {
	var s []string
	for _, f := range l {
		s = append(s, f.Kind[:1]+":"+f.Path)
	}
	return strings.Join(s, ",")
}

var ctx = context.Background()

func TestStatusNotRepo(t *testing.T) {
	needGit(t)
	st, err := Status(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.IsRepo || st.Staged == nil || st.Untracked == nil {
		t.Fatalf("not-repo status = %+v", st)
	}
	if _, ok, err := QuickStatus(ctx, t.TempDir()); ok || err != nil {
		t.Fatalf("QuickStatus on a plain dir: ok=%v err=%v", ok, err)
	}
	if _, err := Log(ctx, t.TempDir(), 10); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("Log outside a repo: %v", err)
	}
}

func TestStatusLists(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "a.txt", "one\n")
	writeFile(t, dir, "b.txt", "bee\n")
	g(t, dir, "add", ".")
	g(t, dir, "commit", "-q", "-m", "two files")

	writeFile(t, dir, "README.md", "hello\nthere\nworld\n") // unstaged: +1
	writeFile(t, dir, "a.txt", "one\ntwo\nthree\n")         // staged +2, then more unstaged
	g(t, dir, "add", "a.txt")
	writeFile(t, dir, "a.txt", "one\ntwo\n")
	g(t, dir, "mv", "b.txt", "c.txt") // staged rename
	writeFile(t, dir, "new/dir/x.txt", "x\n")
	writeFile(t, dir, "sp ace ü.txt", "y\n")

	st, err := Status(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsRepo || st.Branch != "main" || st.Detached || st.LastCommit == nil || st.LastCommit.Subject != "two files" {
		t.Fatalf("status head = %+v", st)
	}
	if got := paths(st.Staged); got != "m:a.txt,r:c.txt" {
		t.Errorf("staged = %s", got)
	}
	if got := paths(st.Unstaged); got != "m:README.md,m:a.txt" {
		t.Errorf("unstaged = %s", got)
	}
	if got := paths(st.Untracked); got != "u:new/dir/x.txt,u:sp ace ü.txt" {
		t.Errorf("untracked = %s", got)
	}
	for _, f := range st.Staged {
		if f.Path == "c.txt" && f.OrigPath != "b.txt" {
			t.Errorf("rename orig = %q", f.OrigPath)
		}
		if f.Path == "a.txt" && (f.Additions != 2 || f.Deletions != 0) {
			t.Errorf("staged a.txt counts = +%d -%d", f.Additions, f.Deletions)
		}
	}
	for _, f := range st.Unstaged {
		if f.Path == "README.md" && (f.Additions != 1 || f.Deletions != 0) {
			t.Errorf("README counts = +%d -%d", f.Additions, f.Deletions)
		}
		if f.Path == "a.txt" && (f.Additions != 0 || f.Deletions != 1) {
			t.Errorf("unstaged a.txt counts = +%d -%d", f.Additions, f.Deletions)
		}
	}
	if st.HasRemote || st.Upstream != "" {
		t.Errorf("remote without one: %+v", st)
	}

	sum, ok, err := QuickStatus(ctx, dir)
	if err != nil || !ok || sum.Branch != "main" || sum.Changes != 5 {
		t.Errorf("QuickStatus = %+v ok=%v err=%v", sum, ok, err)
	}
}

func TestStatusFromSubdirUsesRepoRoot(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "app/web/index.php", "<?php\n")
	writeFile(t, dir, "other.txt", "o\n")
	st, err := Status(ctx, filepath.Join(dir, "app", "web"))
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	gotRoot, _ := filepath.EvalSymlinks(st.Root)
	if gotRoot != real {
		t.Errorf("root = %q, want %q", st.Root, dir)
	}
	if got := paths(st.Untracked); got != "u:app/web/index.php,u:other.txt" {
		t.Errorf("untracked = %s", got)
	}
	// Staging from the subdir takes root-relative paths.
	if err := Stage(ctx, filepath.Join(dir, "app"), []string{"other.txt"}); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if got := paths(st.Staged); got != "a:other.txt" {
		t.Errorf("staged = %s", got)
	}
}

func TestDiff(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "README.md", "hello\nWORLD\n")
	d, err := Diff(ctx, dir, "README.md", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Diff, "-world") || !strings.Contains(d.Diff, "+WORLD") || !strings.Contains(d.Diff, "@@") {
		t.Errorf("unstaged diff:\n%s", d.Diff)
	}
	if d, _ := Diff(ctx, dir, "README.md", true); d.Diff != "" {
		t.Errorf("staged diff before staging:\n%s", d.Diff)
	}
	g(t, dir, "add", "README.md")
	if d, _ := Diff(ctx, dir, "README.md", true); !strings.Contains(d.Diff, "+WORLD") {
		t.Errorf("staged diff:\n%s", d.Diff)
	}
	writeFile(t, dir, "fresh.txt", "a\nb\n")
	d, err = Diff(ctx, dir, "fresh.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Diff, "+a\n+b") || !strings.Contains(d.Diff, "new file") {
		t.Errorf("untracked diff:\n%s", d.Diff)
	}
	// A pathspec-looking name is a literal file name.
	writeFile(t, dir, ":(glob)*", "magic\n")
	if d, err := Diff(ctx, dir, ":(glob)*", false); err != nil || !strings.Contains(d.Diff, "+magic") {
		t.Errorf("literal pathspec diff: %v\n%s", err, d.Diff)
	}
	for _, bad := range []string{"", "../x", "/etc/passwd", "a/../../b"} {
		if _, err := Diff(ctx, dir, bad, false); err == nil {
			t.Errorf("Diff(%q) accepted", bad)
		}
	}
	// Big diffs are capped.
	writeFile(t, dir, "big.txt", strings.Repeat("0123456789abcdef\n", 20000))
	d, err = Diff(ctx, dir, "big.txt", false)
	if err != nil || !d.Truncated || len(d.Diff) > DiffLimit {
		t.Errorf("big diff: err=%v truncated=%v len=%d", err, d.Truncated, len(d.Diff))
	}
}

func TestStageUnstageDiscard(t *testing.T) {
	dir := newRepo(t)
	writeFile(t, dir, "README.md", "changed\n")
	writeFile(t, dir, "new.txt", "n\n")
	writeFile(t, dir, "keep.txt", "k\n")
	if err := Stage(ctx, dir, []string{"README.md", "new.txt"}); err != nil {
		t.Fatal(err)
	}
	st, _ := Status(ctx, dir)
	if got := paths(st.Staged); got != "m:README.md,a:new.txt" {
		t.Fatalf("staged = %s", got)
	}
	if err := Unstage(ctx, dir, []string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if got := paths(st.Staged); got != "m:README.md" {
		t.Fatalf("after unstage = %s", got)
	}
	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if len(st.Staged) != 3 || len(st.Untracked) != 0 {
		t.Fatalf("after stage all = %s / %s", paths(st.Staged), paths(st.Untracked))
	}
	if err := UnstageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if len(st.Staged) != 0 {
		t.Fatalf("after unstage all = %s", paths(st.Staged))
	}

	if err := Discard(ctx, dir, []string{"README.md", "new.txt"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "hello\nworld\n" {
		t.Errorf("README after discard = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked file survived discard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Errorf("unrelated file removed: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, filepath.Dir(outside), "outside.txt", "o\n")
	for _, bad := range []string{outside, "../outside.txt", "..", "."} {
		if err := Discard(ctx, dir, []string{bad}); err == nil {
			t.Errorf("Discard(%q) accepted", bad)
		}
	}
	if runtime.GOOS != "windows" {
		// A symlinked folder pointing outside must not let Discard delete there.
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if err := Discard(ctx, dir, []string{"link/outside.txt"}); err == nil {
			t.Error("Discard through a symlink accepted")
		}
		if _, err := os.Stat(outside); err != nil {
			t.Errorf("file outside the repo was deleted: %v", err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside file touched: %v", err)
	}
}

func TestUnstageWithoutCommits(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	if err := Init(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Init(ctx, dir); err == nil {
		t.Error("Init twice accepted")
	}
	writeFile(t, dir, "a.txt", "a\n")
	writeFile(t, dir, "b.txt", "b\n")
	if err := StageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := Unstage(ctx, dir, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	st, err := Status(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Initial || st.LastCommit != nil || paths(st.Staged) != "a:b.txt" || paths(st.Untracked) != "u:a.txt" {
		t.Fatalf("status = initial %v, staged %s, untracked %s", st.Initial, paths(st.Staged), paths(st.Untracked))
	}
	if err := UnstageAll(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if l, err := Log(ctx, dir, 10); err != nil || len(l) != 0 {
		t.Errorf("Log of empty repo = %v, %v", l, err)
	}
}

func TestCommit(t *testing.T) {
	dir := newRepo(t)
	if _, err := Commit(ctx, dir, "  \n ", false, false); err == nil {
		t.Error("empty message accepted")
	}
	if _, err := Commit(ctx, dir, "nothing staged", false, false); err == nil || !strings.Contains(err.Error(), "nothing to commit") {
		t.Errorf("empty commit: %v", err)
	}
	writeFile(t, dir, "x.txt", "x\n")
	hash, err := Commit(ctx, dir, "Add x\n\nWith a body; $(not a shell) `either`", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) < 40 {
		t.Fatalf("hash = %q", hash)
	}
	if msg := g(t, dir, "log", "-1", "--format=%B"); !strings.Contains(msg, "$(not a shell) `either`") {
		t.Errorf("message = %q", msg)
	}
	writeFile(t, dir, "y.txt", "y\n")
	g(t, dir, "add", "y.txt")
	h2, err := Commit(ctx, dir, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if h2 == hash || strings.TrimSpace(g(t, dir, "log", "-1", "--format=%s")) != "Add x" {
		t.Error("amend without a message should keep the message")
	}
	if n := strings.TrimSpace(g(t, dir, "rev-list", "--count", "HEAD")); n != "2" {
		t.Errorf("commits = %s (amend made a new one?)", n)
	}

	// A rejecting hook's output reaches the caller.
	if runtime.GOOS != "windows" {
		hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
		_ = os.WriteFile(hook, []byte("#!/bin/sh\necho 'lint says no' >&2\nexit 1\n"), 0o755)
		writeFile(t, dir, "z.txt", "z\n")
		if _, err := Commit(ctx, dir, "blocked", false, true); err == nil || !strings.Contains(err.Error(), "lint says no") {
			t.Errorf("hook failure = %v", err)
		}
		os.Remove(hook)
	}

	// No identity → IdentityError.
	bare := t.TempDir()
	g(t, bare, "init", "-q")
	g(t, bare, "config", "user.useConfigOnly", "true")
	writeFile(t, bare, "f", "f\n")
	_, err = Commit(ctx, bare, "who", false, true)
	var ie *IdentityError
	if !errors.As(err, &ie) {
		t.Errorf("identity error = %v", err)
	}
}

func TestPushPullAheadBehind(t *testing.T) {
	dir := newRepo(t)
	bare := t.TempDir()
	g(t, bare, "init", "-q", "--bare", "-b", "main")
	g(t, dir, "remote", "add", "origin", "https://user:s3cret@example.invalid/repo.git")
	st, _ := Status(ctx, dir)
	if strings.Contains(st.RemoteURL, "s3cret") || strings.Contains(st.RemoteURL, "user") || st.Remote != "origin" {
		t.Errorf("remote URL leaks credentials: %q", st.RemoteURL)
	}
	g(t, dir, "remote", "set-url", "origin", bare)

	var streamed strings.Builder
	if err := Push(ctx, dir, true, false, func(b []byte) { streamed.Write(b) }); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if st.Upstream != "origin/main" || st.Ahead != 0 || st.Behind != 0 {
		t.Fatalf("after publish: upstream %q ↑%d ↓%d", st.Upstream, st.Ahead, st.Behind)
	}
	if streamed.Len() == 0 {
		t.Error("push streamed no output")
	}

	// Someone else pushes; we commit locally → diverged.
	other := clone(t, bare)
	writeFile(t, other, "theirs.txt", "t\n")
	g(t, other, "add", ".")
	g(t, other, "commit", "-q", "-m", "theirs")
	g(t, other, "push", "-q")
	writeFile(t, dir, "mine.txt", "m\n")
	if _, err := Commit(ctx, dir, "mine", false, true); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(ctx, dir, nil); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if st.Ahead != 1 || st.Behind != 1 {
		t.Fatalf("diverged: ↑%d ↓%d", st.Ahead, st.Behind)
	}
	if err := Push(ctx, dir, false, false, nil); err == nil {
		t.Error("non-fast-forward push accepted")
	}
	if err := Pull(ctx, dir, "ff-only", nil); err == nil {
		t.Error("ff-only pull of a diverged branch accepted")
	}
	if err := Pull(ctx, dir, "rebase", nil); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if st.Ahead != 1 || st.Behind != 0 || st.RebaseInProgress {
		t.Fatalf("after pull --rebase: ↑%d ↓%d rebase=%v", st.Ahead, st.Behind, st.RebaseInProgress)
	}
	if n := strings.TrimSpace(g(t, dir, "rev-list", "--merges", "--count", "HEAD")); n != "0" {
		t.Errorf("rebase pull made %s merge commits", n)
	}
	if err := Push(ctx, dir, false, false, nil); err != nil {
		t.Fatal(err)
	}

	// Rewrite our last commit and force-push with lease.
	if _, err := Commit(ctx, dir, "mine, reworded", true, false); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, dir, false, false, nil); err == nil {
		t.Error("push of rewritten history accepted without force")
	}
	if err := Push(ctx, dir, false, true, nil); err != nil {
		t.Fatalf("force-with-lease: %v", err)
	}
	if err := Pull(ctx, dir, "bogus", nil); err == nil {
		t.Error("unknown pull mode accepted")
	}
}

func TestMergeConflictAndAbort(t *testing.T) {
	dir := newRepo(t)
	bare := withRemote(t, dir)
	other := clone(t, bare)
	writeFile(t, other, "README.md", "hello\ntheirs\n")
	g(t, other, "commit", "-q", "-am", "theirs")
	g(t, other, "push", "-q")
	writeFile(t, dir, "README.md", "hello\nmine\n")
	g(t, dir, "commit", "-q", "-am", "mine")

	if err := Pull(ctx, dir, "merge", nil); err == nil {
		t.Fatal("conflicting pull succeeded")
	}
	st, _ := Status(ctx, dir)
	if !st.MergeInProgress || paths(st.Conflicted) != "c:README.md" {
		t.Fatalf("merge state: merging=%v conflicted=%s", st.MergeInProgress, paths(st.Conflicted))
	}
	if sum, _, _ := QuickStatus(ctx, dir); sum.Conflicts != 1 {
		t.Errorf("summary conflicts = %d", sum.Conflicts)
	}
	if err := MergeAbort(ctx, dir); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if st.MergeInProgress || len(st.Conflicted) != 0 {
		t.Fatalf("after abort: %+v", st)
	}

	// Again, but resolve and conclude it.
	if err := Pull(ctx, dir, "merge", nil); err == nil {
		t.Fatal("conflicting pull succeeded")
	}
	writeFile(t, dir, "README.md", "hello\nboth\n")
	if err := Stage(ctx, dir, []string{"README.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(ctx, dir, "", false, false); err != nil {
		t.Fatalf("concluding the merge with git's message: %v", err)
	}
	st, _ = Status(ctx, dir)
	if st.MergeInProgress || st.Behind != 0 || st.Ahead != 2 {
		t.Fatalf("after merge: merging=%v ↑%d ↓%d", st.MergeInProgress, st.Ahead, st.Behind)
	}
}

func TestRebaseConflictContinueAbort(t *testing.T) {
	dir := newRepo(t)
	g(t, dir, "switch", "-q", "-c", "feature")
	writeFile(t, dir, "README.md", "hello\nfeature\n")
	g(t, dir, "commit", "-q", "-am", "feature change")
	g(t, dir, "switch", "-q", "main")
	writeFile(t, dir, "README.md", "hello\nmain\n")
	g(t, dir, "commit", "-q", "-am", "main change")
	g(t, dir, "switch", "-q", "feature")

	var out strings.Builder
	if err := Rebase(ctx, dir, "main", func(b []byte) { out.Write(b) }); err == nil {
		t.Fatal("conflicting rebase succeeded")
	}
	st, _ := Status(ctx, dir)
	if !st.RebaseInProgress || len(st.Conflicted) != 1 || st.RebaseBranch != "feature" {
		t.Fatalf("rebase state: %v conflicted=%s branch=%q", st.RebaseInProgress, paths(st.Conflicted), st.RebaseBranch)
	}
	if !strings.Contains(out.String(), "CONFLICT") {
		t.Errorf("rebase output: %s", out.String())
	}
	if err := RebaseAbort(ctx, dir); err != nil {
		t.Fatal(err)
	}
	st, _ = Status(ctx, dir)
	if st.RebaseInProgress || st.Branch != "feature" {
		t.Fatalf("after abort: rebase=%v branch=%q", st.RebaseInProgress, st.Branch)
	}

	if err := Rebase(ctx, dir, "main", nil); err == nil {
		t.Fatal("conflicting rebase succeeded")
	}
	writeFile(t, dir, "README.md", "hello\nmain\nfeature\n")
	if err := Stage(ctx, dir, []string{"README.md"}); err != nil {
		t.Fatal(err)
	}
	if err := RebaseContinue(ctx, dir); err != nil {
		t.Fatalf("continue (must not wait for an editor): %v", err)
	}
	st, _ = Status(ctx, dir)
	if st.RebaseInProgress || st.Branch != "feature" {
		t.Fatalf("after continue: %+v", st)
	}
	if log := g(t, dir, "log", "--format=%s"); !strings.HasPrefix(log, "feature change\nmain change\n") {
		t.Errorf("history after rebase:\n%s", log)
	}
	for _, bad := range []string{"-x", "--exec=sh", "", "no-such-branch", "a b"} {
		if err := Rebase(ctx, dir, bad, nil); err == nil {
			t.Errorf("Rebase(%q) accepted", bad)
		}
	}
}

func TestBranches(t *testing.T) {
	dir := newRepo(t)
	withRemote(t, dir)
	if err := Checkout(ctx, dir, "feature/login", true); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "login.txt", "l\n")
	if _, err := Commit(ctx, dir, "login", false, true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"-f", "has space", "a..b", "x~1", ""} {
		if err := Checkout(ctx, dir, bad, true); err == nil {
			t.Errorf("Checkout(create %q) accepted", bad)
		}
	}
	list, err := Branches(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range list {
		n := b.Name
		if b.Current {
			n = "*" + n
		}
		if b.Remote {
			n = "r:" + n
		}
		names = append(names, n)
	}
	if got := strings.Join(names, ","); got != "*feature/login,main,r:origin/main" {
		t.Fatalf("branches = %s", got)
	}
	if list[1].Upstream != "origin/main" {
		t.Errorf("main upstream = %q", list[1].Upstream)
	}
	if list[2].RemoteName != "origin" || list[2].Short != "main" {
		t.Errorf("remote branch = %+v", list[2])
	}

	if err := Checkout(ctx, dir, "main", false); err != nil {
		t.Fatal(err)
	}
	if err := DeleteBranch(ctx, dir, "feature/login", false); err == nil || !strings.Contains(err.Error(), "force") {
		t.Errorf("unmerged delete: %v", err)
	}
	if err := DeleteBranch(ctx, dir, "feature/login", true); err != nil {
		t.Fatal(err)
	}
	if err := DeleteBranch(ctx, dir, "--all", true); err == nil {
		t.Error("option-looking branch accepted")
	}

	// A branch that only exists on the remote checks out as tracking.
	other := clone(t, filepath.Join(strings.TrimSpace(g(t, dir, "remote", "get-url", "origin"))))
	g(t, other, "switch", "-q", "-c", "remote-only")
	g(t, other, "push", "-q", "-u", "origin", "remote-only")
	if err := Fetch(ctx, dir, nil); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(ctx, dir, "remote-only", false); err != nil {
		t.Fatal(err)
	}
	st, _ := Status(ctx, dir)
	if st.Branch != "remote-only" || st.Upstream != "origin/remote-only" {
		t.Errorf("tracking checkout: %q → %q", st.Branch, st.Upstream)
	}
}

func TestLogAndShow(t *testing.T) {
	dir := newRepo(t)
	withRemote(t, dir)
	writeFile(t, dir, "b.txt", "b\n")
	if _, err := Commit(ctx, dir, "second | with pipe", false, true); err != nil {
		t.Fatal(err)
	}
	g(t, dir, "tag", "v1.0")
	list, err := Log(ctx, dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Subject != "second | with pipe" || list[1].Subject != "initial" || list[0].Author != "Test User" {
		t.Fatalf("log = %+v", list)
	}
	refs := strings.Join(list[0].Refs, ",")
	if !strings.Contains(refs, "HEAD -> main") || !strings.Contains(refs, "tag: v1.0") {
		t.Errorf("refs = %q", refs)
	}
	if !strings.Contains(strings.Join(list[1].Refs, ","), "origin/main") {
		t.Errorf("initial refs = %v", list[1].Refs)
	}
	d, err := Show(ctx, dir, list[0].Short)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Diff, "second | with pipe") || !strings.Contains(d.Diff, "+b") || !strings.Contains(d.Diff, "b.txt |") {
		t.Errorf("show:\n%s", d.Diff)
	}
	for _, bad := range []string{"HEAD", "--output=/tmp/x", "zz", ""} {
		if _, err := Show(ctx, dir, bad); err == nil {
			t.Errorf("Show(%q) accepted", bad)
		}
	}
}

func TestStashes(t *testing.T) {
	dir := newRepo(t)
	if err := StashPush(ctx, dir, "nothing", false); err == nil {
		t.Error("stash of a clean tree accepted")
	}
	writeFile(t, dir, "README.md", "stashed\n")
	writeFile(t, dir, "new.txt", "n\n")
	if err := StashPush(ctx, dir, "wip one", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Error("untracked file stashed without includeUntracked")
	}
	if err := StashPush(ctx, dir, "wip two", true); err != nil {
		t.Fatal(err)
	}
	list, err := StashList(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Index != 0 || !strings.Contains(list[0].Message, "wip two") || !strings.Contains(list[1].Message, "wip one") {
		t.Fatalf("stashes = %+v", list)
	}
	if st, _ := Status(ctx, dir); st.StashCount != 2 || len(st.Untracked) != 0 {
		t.Errorf("status stash count = %d untracked=%d", st.StashCount, len(st.Untracked))
	}
	d, err := StashShow(ctx, dir, 1)
	if err != nil || !strings.Contains(d.Diff, "+stashed") {
		t.Errorf("stash show: %v\n%s", err, d.Diff)
	}
	if err := StashDrop(ctx, dir, 1); err != nil {
		t.Fatal(err)
	}
	if err := StashPop(ctx, dir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Error("pop didn't restore the untracked file")
	}
	if list, _ := StashList(ctx, dir); len(list) != 0 {
		t.Errorf("stashes left = %+v", list)
	}
	if err := StashPop(ctx, dir, -1); err == nil {
		t.Error("negative stash index accepted")
	}
}

func TestCancel(t *testing.T) {
	dir := newRepo(t)
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Status(c, dir); err == nil {
		t.Error("cancelled status succeeded")
	}
}

func TestParseHelpers(t *testing.T) {
	st := parseStatus("# branch.oid abc\x00# branch.head (detached)\x00# branch.upstream origin/x\x00# branch.ab +3 -2\x00# stash 4\x00" +
		"1 .M N... 100644 100644 100644 aa bb one.txt\x00" +
		"2 R. N... 100644 100644 100644 aa bb R100 new name.txt\x00old name.txt\x00" +
		"u UU N... 100644 100644 100644 100644 a b c conflict.txt\x00" +
		"? untracked dir/file.txt\x00! ignored.log\x00")
	if !st.Detached || st.Upstream != "origin/x" || st.Ahead != 3 || st.Behind != 2 || st.StashCount != 4 {
		t.Errorf("header = %+v", st)
	}
	if paths(st.Unstaged) != "m:one.txt" || paths(st.Staged) != "r:new name.txt" || st.Staged[0].OrigPath != "old name.txt" {
		t.Errorf("entries = %s / %s", paths(st.Unstaged), paths(st.Staged))
	}
	if paths(st.Conflicted) != "c:conflict.txt" || paths(st.Untracked) != "u:untracked dir/file.txt" {
		t.Errorf("conflicted/untracked = %s / %s", paths(st.Conflicted), paths(st.Untracked))
	}

	n := parseNumstat("3\t1\ta.txt\x00-\t-\timg.png\x002\t0\t\x00old.txt\x00new.txt\x00")
	if n["a.txt"] != (numstat{add: 3, del: 1}) || !n["img.png"].binary || n["new.txt"].add != 2 {
		t.Errorf("numstat = %+v", n)
	}

	for in, want := range map[string]string{
		"https://user:tok@github.com/a/b.git": "https://github.com/a/b.git",
		"http://tok@host/x":                   "http://host/x",
		"git@github.com:a/b.git":              "git@github.com:a/b.git",
		"ssh://git@host:22/x.git":             "ssh://git@host:22/x.git",
		"ssh://git:pw@host/x.git":             "ssh://git@host/x.git",
		"/srv/repos/x.git":                    "/srv/repos/x.git",
	} {
		if got := StripCredentials(in); got != want {
			t.Errorf("StripCredentials(%q) = %q, want %q", in, got, want)
		}
	}

	for _, out := range []string{
		"fatal: Authentication failed for 'https://github.com/a/b.git/'",
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled",
		"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.",
	} {
		if !isAuthFailure(out) {
			t.Errorf("not an auth failure: %q", out)
		}
	}
	if isAuthFailure("! [rejected] main -> main (non-fast-forward)") {
		t.Error("rejected push taken for an auth failure")
	}

	for _, p := range []string{"a.txt", "dir/b c.txt", "./x", "-dash.txt"} {
		if _, err := ValidPath(p); err != nil {
			t.Errorf("ValidPath(%q): %v", p, err)
		}
	}
	for _, p := range []string{"", "/abs", "../up", "a/../../b", "a\x00b", ".", `\\server\share`} {
		if _, err := ValidPath(p); err == nil {
			t.Errorf("ValidPath(%q) accepted", p)
		}
	}
}

func TestAuthErrorFromNetwork(t *testing.T) {
	needGit(t)
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as git's ssh")
	}
	dir := newRepo(t)
	// A fake ssh that fails like a rejected key.
	fake := filepath.Join(t.TempDir(), "fakessh")
	_ = os.WriteFile(fake, []byte("#!/bin/sh\necho 'git@example.com: Permission denied (publickey).' >&2\nexit 255\n"), 0o755)
	g(t, dir, "config", "core.sshCommand", fake)
	g(t, dir, "remote", "add", "origin", "git@example.com:a/b.git")
	err := Push(ctx, dir, true, false, nil)
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("push with a rejected key = %v", err)
	}
	if !strings.Contains(ae.Error(), "terminal") {
		t.Errorf("auth message = %q", ae.Error())
	}
}

func TestCleanOutput(t *testing.T) {
	out := "Enumerating objects: 7, done.\nCounting objects: 100% (7/7), done.\rCounting objects: 100% (7/7), done.\nFrom /x/remote\n   db05c34..e89008d  main       -> origin/main\nAuto-merging README.md\nCONFLICT (content): Merge conflict in README.md\nAutomatic merge failed; fix conflicts and then commit the result.\nhint: something\n"
	if got := cleanOutput(out); got != "CONFLICT (content): Merge conflict in README.md\nAutomatic merge failed; fix conflicts and then commit the result." {
		t.Errorf("cleanOutput = %q", got)
	}
	if got := cleanOutput("fatal: not a git repository (or any of the parent directories): .git\n"); got != "not a git repository (or any of the parent directories): .git" {
		t.Errorf("cleanOutput = %q", got)
	}
	if got := cleanOutput("Switched to branch 'x'\n"); got != "Switched to branch 'x'" {
		t.Errorf("cleanOutput = %q", got)
	}
}
