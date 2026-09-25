package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

/* ── diffs ───────────────────────────────────────────────────── */

// DiffResult is a unified diff, capped at DiffLimit (Truncated tells).
type DiffResult struct {
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

var binaryRe = regexp.MustCompile(`(?m)^Binary files .* differ$`)

// Diff is one file's unified diff: staged (index vs HEAD) or not
// (working tree vs index). An untracked file shows as all-added.
func Diff(ctx context.Context, dir, path string, staged bool) (DiffResult, error) {
	ri, err := resolve(ctx, dir)
	if err != nil {
		return DiffResult{}, err
	}
	p, err := ValidPath(path)
	if err != nil {
		return DiffResult{}, err
	}
	base := []string{"diff", "--no-color", "--no-ext-diff", "--patch"}
	var args []string
	var ok []int
	switch {
	case staged:
		args = append(base, "--cached", "--", p)
	case !tracked(ctx, ri.root, p) && exists(filepath.Join(ri.root, filepath.FromSlash(p))):
		null := "/dev/null"
		if runtime.GOOS == "windows" {
			null = "NUL"
		}
		// --no-index exits 1 when the files differ (always, here).
		args = append(base, "--no-index", "--", null, p)
		ok = []int{1}
	default:
		args = append(base, "--", p)
	}
	r, err := run(ctx, ri.root, runOpts{read: true, limit: DiffLimit, okCodes: ok, literal: true}, args...)
	if err != nil {
		return DiffResult{}, err
	}
	d := string(r.stdout)
	return DiffResult{Diff: d, Truncated: r.truncated, Binary: binaryRe.MatchString(d)}, nil
}

// tracked reports whether path is in the index.
func tracked(ctx context.Context, root, path string) bool {
	r, err := run(ctx, root, runOpts{read: true, literal: true}, "ls-files", "-z", "--", path)
	return err == nil && len(r.stdout) > 0
}

var hashRe = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// Show is one commit's stat + patch (capped).
func Show(ctx context.Context, dir, hash string) (DiffResult, error) {
	ri, err := resolve(ctx, dir)
	if err != nil {
		return DiffResult{}, err
	}
	if !hashRe.MatchString(hash) {
		return DiffResult{}, fmt.Errorf("%q isn't a commit hash", hash)
	}
	r, err := run(ctx, ri.root, runOpts{read: true, limit: DiffLimit}, "show", "--no-color", "--no-ext-diff", "--stat", "--patch",
		"--format=commit %H%nAuthor: %an <%ae>%nDate:   %ad%n%n%w(0,4,4)%B", hash, "--")
	if err != nil {
		return DiffResult{}, err
	}
	return DiffResult{Diff: string(r.stdout), Truncated: r.truncated}, nil
}

/* ── staging ─────────────────────────────────────────────────── */

func withRoot(ctx context.Context, dir string) (string, error) {
	ri, err := resolve(ctx, dir)
	return ri.root, err
}

// Stage adds paths to the index (deletions included).
func Stage(ctx context.Context, dir string, paths []string) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	ps, err := validPaths(paths)
	if err != nil {
		return err
	}
	_, err = writePaths(ctx, root, append([]string{"add", "-A", "--"}, ps...)...)
	return err
}

// StageAll stages every change, untracked files included.
func StageAll(ctx context.Context, dir string) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	_, err = write(ctx, root, "add", "-A")
	return err
}

func hasHead(ctx context.Context, root string) bool {
	_, err := run(ctx, root, runOpts{read: true}, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}

// Unstage takes paths out of the index (their working-tree changes stay).
func Unstage(ctx context.Context, dir string, paths []string) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	ps, err := validPaths(paths)
	if err != nil {
		return err
	}
	if !hasHead(ctx, root) {
		// No commit yet: nothing to restore from — drop them from the index.
		_, err = writePaths(ctx, root, append([]string{"rm", "--cached", "-r", "-q", "--ignore-unmatch", "--"}, ps...)...)
		return err
	}
	_, err = writePaths(ctx, root, append([]string{"restore", "--staged", "--"}, ps...)...)
	return err
}

// UnstageAll empties the staging area.
func UnstageAll(ctx context.Context, dir string) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	if !hasHead(ctx, root) {
		_, err = write(ctx, root, "rm", "--cached", "-r", "-q", "--ignore-unmatch", "--", ".")
		return err
	}
	_, err = write(ctx, root, "reset", "-q")
	return err
}

// Discard throws working-tree changes away: tracked files go back to
// their staged (or committed) content, untracked ones are deleted.
// Only paths inside the repository are accepted.
func Discard(ctx context.Context, dir string, paths []string) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	ps, err := validPaths(paths)
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	var trackedPaths []string
	for _, p := range ps {
		if tracked(ctx, root, p) {
			trackedPaths = append(trackedPaths, p)
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		// The parent must really be inside the repo (a symlinked folder
		// pointing elsewhere must not let us delete outside it).
		parent, err := filepath.EvalSymlinks(filepath.Dir(full))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		rel, err := filepath.Rel(realRoot, parent)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%q is outside the project", p)
		}
		target := filepath.Join(parent, filepath.Base(full))
		if target == realRoot || filepath.Base(full) == ".git" {
			return fmt.Errorf("refusing to delete %q", p)
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	if len(trackedPaths) > 0 {
		if _, err := writePaths(ctx, root, append([]string{"restore", "--worktree", "--"}, trackedPaths...)...); err != nil {
			return err
		}
	}
	return nil
}

/* ── commits ─────────────────────────────────────────────────── */

// Commit records the staged changes and returns the new commit's hash.
// An empty message is refused, except to amend (keeps the message) or
// to conclude a merge (uses git's prepared message). Hook output is part
// of the error when a hook rejects the commit.
func Commit(ctx context.Context, dir, message string, amend, all bool) (string, error) {
	ri, err := resolve(ctx, dir)
	if err != nil {
		return "", err
	}
	root := ri.root
	msg := strings.TrimSpace(strings.ReplaceAll(message, "\r\n", "\n"))
	merging := exists(filepath.Join(ri.gitDir, "MERGE_HEAD"))
	if msg == "" && !amend && !merging {
		return "", errors.New("write a commit message first")
	}
	if all {
		if _, err := write(ctx, root, "add", "-A"); err != nil {
			return "", err
		}
	}
	args := []string{"commit"}
	if amend {
		args = append(args, "--amend")
	}
	o := runOpts{}
	if msg == "" {
		args = append(args, "--no-edit")
	} else {
		args = append(args, "-F", "-")
		o.stdin = msg + "\n"
	}
	r, err := run(ctx, root, o, args...)
	if err != nil {
		out := r.stderr + string(r.stdout)
		l := strings.ToLower(out)
		switch {
		case strings.Contains(l, "author identity unknown") || strings.Contains(l, "please tell me who you are") ||
			strings.Contains(l, "unable to auto-detect email address") || strings.Contains(l, "empty ident name"):
			return "", &IdentityError{Output: out}
		case strings.Contains(l, "nothing to commit") || strings.Contains(l, "no changes added to commit") || strings.Contains(l, "nothing added to commit"):
			return "", errors.New("nothing to commit — stage some changes first")
		}
		return "", err
	}
	hash, err := output(ctx, root, "rev-parse", "HEAD")
	return strings.TrimSpace(hash), err
}

/* ── network ─────────────────────────────────────────────────── */

// Pull brings the upstream's commits in: mode "merge" (default),
// "rebase" or "ff-only".
func Pull(ctx context.Context, dir, mode string, onOutput func([]byte)) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	args := []string{"pull", "--progress"}
	switch mode {
	case "", "merge":
		args = append(args, "--no-rebase", "--no-edit")
	case "rebase":
		args = append(args, "--rebase")
	case "ff-only":
		args = append(args, "--ff-only")
	default:
		return fmt.Errorf("unknown pull mode %q (merge, rebase or ff-only)", mode)
	}
	return network(ctx, root, "pull", onOutput, args...)
}

// Push sends the current branch. setUpstream publishes it to the
// primary remote (origin) under the same name; force uses
// --force-with-lease only (never a blind --force).
func Push(ctx context.Context, dir string, setUpstream, force bool, onOutput func([]byte)) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	args := []string{"push", "--progress"}
	if force {
		args = append(args, "--force-with-lease")
	}
	if setUpstream {
		remote, _, ok := primaryRemote(ctx, root)
		if !ok {
			return errors.New("this repository has no remote yet — add one first (git remote add origin <url>)")
		}
		branch, err := output(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil || strings.TrimSpace(branch) == "" {
			return errors.New("you're not on a branch (detached HEAD) — create or check out a branch to publish it")
		}
		args = append(args, "--set-upstream", remote, "HEAD")
	}
	return network(ctx, root, "push", onOutput, args...)
}

// Fetch updates every remote's branches.
func Fetch(ctx context.Context, dir string, onOutput func([]byte)) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	return network(ctx, root, "fetch", onOutput, "fetch", "--all", "--progress")
}

/* ── branches ────────────────────────────────────────────────── */

// Branch is a local or remote-tracking branch.
type Branch struct {
	Name          string `json:"name"`
	Current       bool   `json:"current"`
	Upstream      string `json:"upstream"`
	Ahead         int    `json:"ahead"`
	Behind        int    `json:"behind"`
	Gone          bool   `json:"gone,omitempty"`
	LastCommitRel string `json:"lastCommitRel"`
	Remote        bool   `json:"remote"`
	// RemoteName is the remote a remote-tracking branch belongs to;
	// Short is its name without it (what checking it out creates).
	RemoteName string `json:"remoteName,omitempty"`
	Short      string `json:"short,omitempty"`
	unix       int64
}

var trackRe = regexp.MustCompile(`(ahead|behind) (\d+)`)

// Branches lists local branches (current first, then most recent) and
// remote-tracking ones.
func Branches(ctx context.Context, dir string) ([]Branch, error) {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return nil, err
	}
	out, err := output(ctx, root, "for-each-ref",
		"--format=%(refname)%00%(refname:short)%00%(HEAD)%00%(upstream:short)%00%(upstream:track,nobracket)%00%(committerdate:relative)%00%(committerdate:unix)",
		"refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	list := []Branch{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 7 {
			continue
		}
		b := Branch{Name: f[1], Current: f[2] == "*", Upstream: f[3], LastCommitRel: f[5]}
		b.unix, _ = strconv.ParseInt(f[6], 10, 64)
		if strings.HasPrefix(f[0], "refs/remotes/") {
			rest := strings.TrimPrefix(f[0], "refs/remotes/")
			remote, short, found := strings.Cut(rest, "/")
			if !found || short == "HEAD" {
				continue
			}
			b.Remote, b.RemoteName, b.Short = true, remote, short
		}
		if f[4] == "gone" {
			b.Gone = true
		}
		for _, m := range trackRe.FindAllStringSubmatch(f[4], -1) {
			n, _ := strconv.Atoi(m[2])
			if m[1] == "ahead" {
				b.Ahead = n
			} else {
				b.Behind = n
			}
		}
		list = append(list, b)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Remote != b.Remote {
			return !a.Remote
		}
		if a.Current != b.Current {
			return a.Current
		}
		return a.unix > b.unix
	})
	return list, nil
}

// Checkout switches to branch; create makes it first (from HEAD). A
// name that only exists on a remote creates a local tracking branch.
func Checkout(ctx context.Context, dir, branch string, create bool) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	if create {
		if err := ValidBranchName(ctx, root, branch); err != nil {
			return err
		}
		_, err = write(ctx, root, "switch", "-c", branch)
		return err
	}
	if err := validRefArg(branch); err != nil {
		return err
	}
	_, err = write(ctx, root, "switch", branch)
	return err
}

// DeleteBranch deletes a local branch; without force git refuses to
// drop unmerged work.
func DeleteBranch(ctx context.Context, dir, name string, force bool) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	if err := validRefArg(name); err != nil {
		return err
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err = write(ctx, root, "branch", flag, "--", name)
	if err != nil && !force && strings.Contains(err.Error(), "not fully merged") {
		return fmt.Errorf("%s has commits that aren't merged anywhere — delete it anyway with force", name)
	}
	return err
}

/* ── rebase / merge ──────────────────────────────────────────── */

// Rebase replays the current branch onto another ref.
func Rebase(ctx context.Context, dir, onto string, onOutput func([]byte)) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	if err := validRefArg(onto); err != nil {
		return err
	}
	if _, err := run(ctx, root, runOpts{read: true}, "rev-parse", "--verify", "--quiet", onto+"^{commit}"); err != nil {
		return fmt.Errorf("there's no branch or commit named %q", onto)
	}
	_, err = run(ctx, root, runOpts{onOutput: onOutput}, "rebase", onto)
	return err
}

// RebaseContinue continues a rebase after conflicts are resolved (and
// staged); RebaseSkip drops the current commit; RebaseAbort goes back.
func RebaseContinue(ctx context.Context, dir string) error {
	return simple(ctx, dir, "rebase", "--continue")
}
func RebaseSkip(ctx context.Context, dir string) error  { return simple(ctx, dir, "rebase", "--skip") }
func RebaseAbort(ctx context.Context, dir string) error { return simple(ctx, dir, "rebase", "--abort") }

// MergeAbort abandons a conflicted merge; MergeContinue concludes it
// once every conflict is staged (with git's prepared message).
func MergeAbort(ctx context.Context, dir string) error { return simple(ctx, dir, "merge", "--abort") }
func MergeContinue(ctx context.Context, dir string) error {
	return simple(ctx, dir, "commit", "--no-edit")
}

// CherryPickAbort abandons a conflicted cherry-pick.
func CherryPickAbort(ctx context.Context, dir string) error {
	return simple(ctx, dir, "cherry-pick", "--abort")
}

func simple(ctx context.Context, dir string, args ...string) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	_, err = write(ctx, root, args...)
	return err
}

/* ── history ─────────────────────────────────────────────────── */

// LogEntry is one history entry.
type LogEntry struct {
	Hash    string   `json:"hash"`
	Short   string   `json:"short"`
	Subject string   `json:"subject"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Rel     string   `json:"rel"`
	Refs    []string `json:"refs"`
}

// Log lists the last limit commits of HEAD (empty for a new repo).
func Log(ctx context.Context, dir string, limit int) ([]LogEntry, error) {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	list := []LogEntry{}
	if !hasHead(ctx, root) {
		return list, nil
	}
	out, err := output(ctx, root, "log", "-n", strconv.Itoa(limit), "--decorate=short",
		"--format=%H%x00%h%x00%s%x00%an%x00%aI%x00%ar%x00%D%x1e")
	if err != nil {
		return nil, err
	}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.Split(rec, "\x00")
		if len(f) < 7 {
			continue
		}
		e := LogEntry{Hash: f[0], Short: f[1], Subject: f[2], Author: f[3], Date: f[4], Rel: f[5], Refs: []string{}}
		for _, r := range strings.Split(f[6], ", ") {
			if r = strings.TrimSpace(r); r != "" {
				e.Refs = append(e.Refs, r)
			}
		}
		list = append(list, e)
	}
	return list, nil
}

/* ── stashes ─────────────────────────────────────────────────── */

// Stash is one stash entry.
type Stash struct {
	Index   int    `json:"index"`
	Ref     string `json:"ref"`
	Message string `json:"message"`
	Rel     string `json:"rel"`
}

var stashRefRe = regexp.MustCompile(`^stash@\{(\d+)\}$`)

// StashList lists the stashes, newest first.
func StashList(ctx context.Context, dir string) ([]Stash, error) {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return nil, err
	}
	out, err := output(ctx, root, "stash", "list", "--format=%gd%x00%gs%x00%cr")
	if err != nil {
		return nil, err
	}
	list := []Stash{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 3 {
			continue
		}
		m := stashRefRe.FindStringSubmatch(f[0])
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		list = append(list, Stash{Index: n, Ref: f[0], Message: f[1], Rel: f[2]})
	}
	return list, nil
}

// StashPush stashes the working tree (and index); includeUntracked
// takes new files too.
func StashPush(ctx context.Context, dir, message string, includeUntracked bool) error {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return err
	}
	args := []string{"stash", "push"}
	if includeUntracked {
		args = append(args, "--include-untracked")
	}
	if m := strings.TrimSpace(strings.ReplaceAll(message, "\n", " ")); m != "" {
		args = append(args, "-m", m)
	}
	out, err := write(ctx, root, args...)
	if err != nil {
		return err
	}
	if strings.Contains(out, "No local changes to save") {
		return errors.New("nothing to stash — there are no local changes")
	}
	return nil
}

func stashRef(index int) (string, error) {
	if index < 0 || index > 10000 {
		return "", fmt.Errorf("no stash #%d", index)
	}
	return "stash@{" + strconv.Itoa(index) + "}", nil
}

// StashPop applies a stash and drops it (kept when it conflicts).
func StashPop(ctx context.Context, dir string, index int) error {
	ref, err := stashRef(index)
	if err != nil {
		return err
	}
	return simple(ctx, dir, "stash", "pop", ref)
}

// StashApply applies a stash and keeps it.
func StashApply(ctx context.Context, dir string, index int) error {
	ref, err := stashRef(index)
	if err != nil {
		return err
	}
	return simple(ctx, dir, "stash", "apply", ref)
}

// StashDrop deletes a stash.
func StashDrop(ctx context.Context, dir string, index int) error {
	ref, err := stashRef(index)
	if err != nil {
		return err
	}
	return simple(ctx, dir, "stash", "drop", ref)
}

// StashShow is a stash's patch (capped).
func StashShow(ctx context.Context, dir string, index int) (DiffResult, error) {
	root, err := withRoot(ctx, dir)
	if err != nil {
		return DiffResult{}, err
	}
	ref, err := stashRef(index)
	if err != nil {
		return DiffResult{}, err
	}
	r, err := run(ctx, root, runOpts{read: true, limit: DiffLimit}, "stash", "show", "--no-color", "--stat", "--patch", "--include-untracked", ref)
	if err != nil {
		// Older gits don't know --include-untracked on show.
		r, err = run(ctx, root, runOpts{read: true, limit: DiffLimit}, "stash", "show", "--no-color", "--stat", "--patch", ref)
		if err != nil {
			return DiffResult{}, err
		}
	}
	return DiffResult{Diff: string(r.stdout), Truncated: r.truncated}, nil
}

/* ── init ────────────────────────────────────────────────────── */

// Init makes dir a new repository (refused when it's already in one).
func Init(ctx context.Context, dir string) error {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return errors.New("the project folder doesn't exist: " + dir)
	}
	if _, err := resolve(ctx, dir); err == nil {
		return errors.New("this folder is already in a Git repository")
	} else if !errors.Is(err, ErrNotRepo) {
		return err
	}
	_, err := write(ctx, dir, "init")
	return err
}
