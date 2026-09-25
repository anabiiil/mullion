package gitops

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// FileChange is one changed file in a status list.
type FileChange struct {
	Path     string `json:"path"`
	OrigPath string `json:"origPath,omitempty"`
	X        string `json:"x"`
	Y        string `json:"y"`
	// Kind: modified, added, deleted, renamed, untracked or conflict.
	Kind      string `json:"kind"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary,omitempty"`
}

// CommitInfo is a short description of one commit.
type CommitInfo struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	RelTime string `json:"relTime"`
}

// RepoStatus is everything the Git tab shows about a working tree.
type RepoStatus struct {
	IsRepo bool `json:"isRepo"`
	// Root is the repository's top-level folder — every path below is
	// relative to it (it's the project folder, or one of its parents).
	Root     string `json:"root,omitempty"`
	Branch   string `json:"branch"`
	Detached bool   `json:"detached"`
	// Initial: the branch has no commits yet.
	Initial  bool   `json:"initial"`
	Head     string `json:"head,omitempty"`
	Upstream string `json:"upstream"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`

	Staged     []FileChange `json:"staged"`
	Unstaged   []FileChange `json:"unstaged"`
	Untracked  []FileChange `json:"untracked"`
	Conflicted []FileChange `json:"conflicted"`
	// Truncated: a list was cut at MaxStatusEntries.
	Truncated bool `json:"truncated,omitempty"`

	StashCount int         `json:"stashCount"`
	LastCommit *CommitInfo `json:"lastCommit"`
	Remote     string      `json:"remote"`
	RemoteURL  string      `json:"remoteURL"`
	HasRemote  bool        `json:"hasRemote"`

	MergeInProgress      bool `json:"mergeInProgress"`
	RebaseInProgress     bool `json:"rebaseInProgress"`
	CherryPickInProgress bool `json:"cherryPickInProgress"`
	// RebaseBranch is the branch being rebased (HEAD is detached while a
	// rebase runs).
	RebaseBranch string `json:"rebaseBranch,omitempty"`
}

// MaxStatusEntries caps each file list (an un-ignored node_modules must
// not produce a 100k-row page).
const MaxStatusEntries = 1000

// repoInfo is what rev-parse tells us about dir.
type repoInfo struct {
	root   string
	gitDir string
}

// isNotRepo reports whether err is git's "not a git repository".
func isNotRepo(err error) bool {
	var ge *Error
	if errors.As(err, &ge) {
		return strings.Contains(strings.ToLower(ge.Output), "not a git repository")
	}
	return false
}

// resolve finds dir's repository (root and git dir). ErrNotRepo when
// dir isn't inside one.
func resolve(ctx context.Context, dir string) (repoInfo, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return repoInfo{}, errors.New("the project folder doesn't exist: " + dir)
	}
	out, err := output(ctx, dir, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		if isNotRepo(err) {
			return repoInfo{}, ErrNotRepo
		}
		return repoInfo{}, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || lines[0] == "" {
		// A bare repository, or the inside of a .git dir.
		return repoInfo{}, ErrNotRepo
	}
	root := filepath.FromSlash(strings.TrimSpace(lines[0]))
	// git reports the resolved path (/private/tmp/x for /tmp/x on macOS);
	// keep the caller's spelling when it's the same folder.
	if real, err := filepath.EvalSymlinks(dir); err == nil && samePath(real, root) {
		root = dir
	}
	return repoInfo{root: root, gitDir: filepath.FromSlash(strings.TrimSpace(lines[1]))}, nil
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Root returns dir's repository top-level (ErrNotRepo when it has none).
func Root(ctx context.Context, dir string) (string, error) {
	ri, err := resolve(ctx, dir)
	return ri.root, err
}

// HasRepoMarker is a cheap, process-free check: is there a .git (dir or
// worktree file) in dir or any parent? False means dir surely isn't in
// a repository (true can still be a false positive).
func HasRepoMarker(dir string) bool {
	d, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// Status reads the working tree's state. A folder outside any
// repository is not an error: it returns Status{IsRepo: false}.
func Status(ctx context.Context, dir string) (RepoStatus, error) {
	ri, err := resolve(ctx, dir)
	if errors.Is(err, ErrNotRepo) {
		return RepoStatus{IsRepo: false, Staged: []FileChange{}, Unstaged: []FileChange{}, Untracked: []FileChange{}, Conflicted: []FileChange{}}, nil
	}
	if err != nil {
		return RepoStatus{}, err
	}
	root := ri.root

	var (
		wg                     sync.WaitGroup
		statusOut              string
		statusErr              error
		unstagedNum, stagedNum map[string]numstat
		last                   *CommitInfo
		remote, remoteURL      string
		hasRemote              bool
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		statusOut, statusErr = output(ctx, root, "status", "--porcelain=v2", "--branch", "--show-stash", "-z", "--untracked-files=all")
	}()
	go func() {
		defer wg.Done()
		unstagedNum = readNumstat(ctx, root, false)
		stagedNum = readNumstat(ctx, root, true)
	}()
	go func() {
		defer wg.Done()
		out, err := output(ctx, root, "log", "-1", "--format=%H%x00%s%x00%an%x00%ar")
		if err == nil {
			f := strings.Split(strings.TrimRight(out, "\n"), "\x00")
			if len(f) >= 4 {
				last = &CommitInfo{Hash: f[0], Subject: f[1], Author: f[2], RelTime: f[3]}
			}
		}
	}()
	go func() {
		defer wg.Done()
		remote, remoteURL, hasRemote = primaryRemote(ctx, root)
	}()
	wg.Wait()
	if statusErr != nil {
		return RepoStatus{}, statusErr
	}

	st := parseStatus(statusOut)
	st.IsRepo = true
	st.Root = root
	st.LastCommit = last
	st.Remote, st.RemoteURL, st.HasRemote = remote, remoteURL, hasRemote
	for i := range st.Staged {
		applyNum(&st.Staged[i], stagedNum)
	}
	for i := range st.Unstaged {
		applyNum(&st.Unstaged[i], unstagedNum)
	}
	st.MergeInProgress = exists(filepath.Join(ri.gitDir, "MERGE_HEAD"))
	st.RebaseInProgress = exists(filepath.Join(ri.gitDir, "rebase-merge")) || exists(filepath.Join(ri.gitDir, "rebase-apply"))
	st.CherryPickInProgress = exists(filepath.Join(ri.gitDir, "CHERRY_PICK_HEAD"))
	if st.RebaseInProgress {
		for _, d := range []string{"rebase-merge", "rebase-apply"} {
			if b, err := os.ReadFile(filepath.Join(ri.gitDir, d, "head-name")); err == nil {
				st.RebaseBranch = strings.TrimPrefix(strings.TrimSpace(string(b)), "refs/heads/")
				break
			}
		}
	}
	return st, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func applyNum(f *FileChange, m map[string]numstat) {
	if n, ok := m[f.Path]; ok {
		f.Additions, f.Deletions, f.Binary = n.add, n.del, n.binary
	}
}

// parseStatus parses `git status --porcelain=v2 --branch --show-stash -z`.
func parseStatus(out string) RepoStatus {
	st := RepoStatus{Staged: []FileChange{}, Unstaged: []FileChange{}, Untracked: []FileChange{}, Conflicted: []FileChange{}}
	recs := strings.Split(out, "\x00")
	add := func(list *[]FileChange, f FileChange) {
		if len(*list) >= MaxStatusEntries {
			st.Truncated = true
			return
		}
		*list = append(*list, f)
	}
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r == "" {
			continue
		}
		switch r[0] {
		case '#':
			key, val, _ := strings.Cut(strings.TrimPrefix(r, "# "), " ")
			switch key {
			case "branch.oid":
				if val == "(initial)" {
					st.Initial = true
				} else {
					st.Head = val
				}
			case "branch.head":
				if val == "(detached)" {
					st.Detached = true
				} else {
					st.Branch = val
				}
			case "branch.upstream":
				st.Upstream = val
			case "branch.ab":
				for _, p := range strings.Fields(val) {
					n, _ := strconv.Atoi(p[1:])
					if p[0] == '+' {
						st.Ahead = n
					} else if p[0] == '-' {
						st.Behind = n
					}
				}
			case "stash":
				st.StashCount, _ = strconv.Atoi(val)
			}
		case '1', '2':
			// 1 XY sub mH mI mW hH hI path
			// 2 XY sub mH mI mW hH hI Xscore path \0 origPath
			n := 9
			if r[0] == '2' {
				n = 10
			}
			f := strings.SplitN(r, " ", n)
			if len(f) < n {
				continue
			}
			xy, path := f[1], f[n-1]
			orig := ""
			if r[0] == '2' && i+1 < len(recs) {
				i++
				orig = recs[i]
			}
			x, y := xy[0], xy[1]
			if x != '.' {
				fc := FileChange{Path: path, X: string(x), Y: string(y), Kind: kindOf(x)}
				if r[0] == '2' {
					fc.OrigPath = orig
				}
				add(&st.Staged, fc)
			}
			if y != '.' {
				add(&st.Unstaged, FileChange{Path: path, X: string(x), Y: string(y), Kind: kindOf(y)})
			}
		case 'u':
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			f := strings.SplitN(r, " ", 11)
			if len(f) < 11 {
				continue
			}
			add(&st.Conflicted, FileChange{Path: f[10], X: f[1][:1], Y: f[1][1:2], Kind: "conflict"})
		case '?':
			add(&st.Untracked, FileChange{Path: strings.TrimPrefix(r, "? "), X: "?", Y: "?", Kind: "untracked"})
		}
	}
	return st
}

func kindOf(c byte) string {
	switch c {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R', 'C':
		return "renamed"
	}
	return "modified"
}

type numstat struct {
	add, del int
	binary   bool
}

// readNumstat maps path → +/− counts for unstaged (or staged) changes.
func readNumstat(ctx context.Context, root string, staged bool) map[string]numstat {
	args := []string{"diff", "--numstat", "-z", "--no-ext-diff", "--no-textconv"}
	if staged {
		args = append(args, "--cached")
	}
	out, err := output(ctx, root, args...)
	m := map[string]numstat{}
	if err != nil {
		return m
	}
	return parseNumstat(out)
}

// parseNumstat parses `git diff --numstat -z`: "add\tdel\tpath\0", or
// for a rename "add\tdel\t\0orig\0path\0"; binary files show "-\t-".
func parseNumstat(out string) map[string]numstat {
	m := map[string]numstat{}
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		if r == "" {
			continue
		}
		f := strings.SplitN(r, "\t", 3)
		if len(f) < 3 {
			continue
		}
		path := f[2]
		if path == "" && i+2 < len(recs) {
			path = recs[i+2]
			i += 2
		}
		var n numstat
		if f[0] == "-" {
			n.binary = true
		} else {
			n.add, _ = strconv.Atoi(f[0])
			n.del, _ = strconv.Atoi(f[1])
		}
		m[path] = n
	}
	return m
}

// primaryRemote picks "origin", else the first remote, and its URL
// with any credentials removed.
func primaryRemote(ctx context.Context, root string) (name, u string, ok bool) {
	out, err := output(ctx, root, "remote")
	if err != nil {
		return "", "", false
	}
	names := strings.Fields(out)
	if len(names) == 0 {
		return "", "", false
	}
	name = names[0]
	for _, n := range names {
		if n == "origin" {
			name = n
		}
	}
	raw, err := output(ctx, root, "remote", "get-url", "--", name)
	if err != nil {
		return name, "", true
	}
	return name, StripCredentials(strings.TrimSpace(raw)), true
}

// StripCredentials drops the user:password@ part of an http(s) remote
// URL (a token pasted into a URL must never reach the page). SSH-style
// "git@host:path" URLs keep their user, which isn't a secret.
func StripCredentials(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable: cut anything between "://" and the last "@".
		i := strings.Index(raw, "://")
		if at := strings.LastIndex(raw, "@"); at > i {
			return raw[:i+3] + raw[at+1:]
		}
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ftp", "ftps":
		u.User = nil
	default:
		if u.User != nil {
			u.User = url.User(u.User.Username())
		}
	}
	return u.String()
}

// Summary is the cheap status the Sites page shows on a card.
type Summary struct {
	Branch    string `json:"branch"`
	Detached  bool   `json:"detached"`
	Changes   int    `json:"changes"`
	Conflicts int    `json:"conflicts"`
	Ahead     int    `json:"ahead"`
	Behind    int    `json:"behind"`
	Upstream  string `json:"upstream,omitempty"`
}

// QuickStatus is one `git status` for a card: branch + counts. ok is
// false when dir isn't in a repository.
func QuickStatus(ctx context.Context, dir string) (sum Summary, ok bool, err error) {
	if !HasRepoMarker(dir) {
		return Summary{}, false, nil
	}
	out, err := output(ctx, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal")
	if err != nil {
		if isNotRepo(err) {
			return Summary{}, false, nil
		}
		return Summary{}, false, err
	}
	st := parseStatus(out)
	sum = Summary{Branch: st.Branch, Detached: st.Detached, Ahead: st.Ahead, Behind: st.Behind, Upstream: st.Upstream,
		Conflicts: len(st.Conflicted)}
	// Count files, not (index, worktree) pairs.
	seen := map[string]bool{}
	for _, l := range [][]FileChange{st.Staged, st.Unstaged, st.Untracked, st.Conflicted} {
		for _, f := range l {
			seen[f.Path] = true
		}
	}
	sum.Changes = len(seen)
	if st.Detached && st.Head != "" && len(st.Head) >= 7 {
		sum.Branch = st.Head[:7]
	}
	return sum, true, nil
}
