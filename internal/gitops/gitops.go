// Package gitops drives the user's own `git` for the project page's Git
// tab: status, diffs, staging, commits, branches, stashes and the
// network operations (fetch / pull / push).
//
// Every call shells out to git with an argv (never a shell), always
// with -C <repo>, a fixed locale (LC_ALL=C, so output can be parsed),
// no terminal prompts (GIT_TERMINAL_PROMPT=0 — the panel has no TTY to
// answer them on) and a no-op editor (a merge commit or a rebase must
// never wait for an editor that will never open). Reads run with
// GIT_OPTIONAL_LOCKS=0 so a 4-second status poll can't fight a commit
// for index.lock. User-supplied refs are validated and paths are always
// passed after "--" as literal pathspecs.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"pm/internal/proc"
)

// Timeouts: local reads/writes are quick; network operations get two
// minutes (a big first fetch over a slow link).
const (
	ReadTimeout    = 10 * time.Second
	WriteTimeout   = 60 * time.Second
	NetworkTimeout = 2 * time.Minute
)

// DiffLimit caps a diff (or a commit's patch) handed to the page.
const DiffLimit = 200 << 10

// ErrNotInstalled is returned when no git binary is on PATH.
var ErrNotInstalled = errors.New(notInstalledMessage())

func notInstalledMessage() string {
	switch runtime.GOOS {
	case "darwin":
		return "Git isn't installed — install Apple's command line tools (run `xcode-select --install` in a terminal) or `brew install git`, then try again"
	case "windows":
		return "Git isn't installed — install Git for Windows (https://git-scm.com/download/win), then restart Mullion"
	}
	return "Git isn't installed — install it with your package manager, then try again"
}

// ErrNotRepo is returned by operations that need a repository when the
// folder isn't one.
var ErrNotRepo = errors.New("this folder isn't a Git repository")

// Error is a failed git command: its (trimmed) output explains why.
type Error struct {
	Args   []string
	Output string
	Err    error
}

func (e *Error) Error() string {
	msg := cleanOutput(e.Output)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		msg = "failed"
	}
	name := "git"
	if len(e.Args) > 0 {
		name += " " + e.Args[0]
	}
	return name + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// AuthError is a network operation that failed to authenticate — the
// panel can't answer a password or passphrase prompt, so the user signs
// in once in a terminal (after which the credential helper / ssh agent
// has it).
type AuthError struct {
	Op     string
	Output string
}

func (e *AuthError) Error() string {
	return "git " + e.Op + " couldn't sign in to the remote — run it in the terminal once to sign in (your credential helper or SSH agent remembers it afterwards)"
}

// IdentityError is a commit refused because git doesn't know who the
// author is.
type IdentityError struct{ Output string }

func (e *IdentityError) Error() string {
	return "Git doesn't know who you are yet — set your name and email once in a terminal: git config --global user.name \"Your Name\" && git config --global user.email you@example.com"
}

var authPatterns = []string{
	"authentication failed",
	"could not read username",
	"could not read password",
	"permission denied (publickey",
	"terminal prompts disabled",
	"host key verification failed",
	"the requested url returned error: 401",
	"the requested url returned error: 403",
	"invalid username or password",
	"support for password authentication was removed",
}

func isAuthFailure(out string) bool {
	l := strings.ToLower(out)
	for _, p := range authPatterns {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// progressPrefixes start the transfer chatter of fetch/pull/push.
var progressPrefixes = []string{
	"Enumerating objects", "Counting objects", "Compressing objects", "Writing objects",
	"Receiving objects", "Resolving deltas", "Unpacking objects", "Delta compression",
	"Total ", "From ", "To ", "remote: Enumerating", "remote: Counting", "remote: Compressing", "remote: Total",
}

// importantPrefixes mark the lines that say what went wrong.
var importantPrefixes = []string{
	"fatal:", "error:", "CONFLICT", "! [rejected]", "! [remote rejected]", "Could not apply",
	"Automatic merge failed", "Please commit your changes or stash them", "Aborting",
}

// cleanOutput turns git's output into a short message: when some lines
// say what failed (fatal:, error:, CONFLICT…) only those are kept;
// otherwise progress and hint: lines are dropped. "fatal: " / "error: "
// prefixes are removed.
func cleanOutput(out string) string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	var keep, important []string
	for _, line := range strings.Split(out, "\n") {
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			line = line[i+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "hint:") {
			continue
		}
		progress := false
		for _, p := range progressPrefixes {
			if strings.HasPrefix(line, p) {
				progress = true
				break
			}
		}
		if progress || (strings.Contains(line, "->") && strings.Contains(line, "..")) {
			continue
		}
		imp := false
		for _, p := range importantPrefixes {
			if strings.HasPrefix(line, p) {
				imp = true
				break
			}
		}
		for _, p := range []string{"fatal: ", "error: ", "remote: "} {
			line = strings.TrimPrefix(line, p)
		}
		keep = append(keep, line)
		if imp {
			important = append(important, line)
		}
	}
	if len(important) > 0 {
		keep = important
	}
	msg := strings.Join(keep, "\n")
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	return msg
}

var (
	gitPathOnce sync.Once
	gitPath     string
	gitPathErr  error
)

// gitBinary finds git once per process.
func gitBinary() (string, error) {
	gitPathOnce.Do(func() {
		gitPath, gitPathErr = exec.LookPath("git")
		if gitPathErr != nil {
			gitPathErr = ErrNotInstalled
		}
	})
	return gitPath, gitPathErr
}

// Available reports whether git is installed.
func Available() bool {
	_, err := gitBinary()
	return err == nil
}

// runOpts tunes one git invocation.
type runOpts struct {
	read     bool          // GIT_OPTIONAL_LOCKS=0
	timeout  time.Duration // 0 → ReadTimeout for reads, WriteTimeout otherwise
	stdin    string
	onOutput func([]byte) // streams stdout+stderr as it arrives
	limit    int          // cap captured stdout (0 = unlimited)
	okCodes  []int        // extra exit codes that count as success
	// literal: the paths passed are file names, never pathspec patterns
	// (":(glob)*" or ":/" must not mean anything special). Not global:
	// git stash and friends use pathspec magic internally.
	literal bool
}

// result is a finished command.
type result struct {
	stdout    []byte
	stderr    string
	truncated bool
	code      int
}

// limitBuffer keeps at most max bytes and remembers it dropped some.
type limitBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if l.max > 0 {
		room := l.max - l.buf.Len()
		if room <= 0 {
			l.truncated = true
			return len(p), nil
		}
		if len(p) > room {
			l.buf.Write(p[:room])
			l.truncated = true
			return len(p), nil
		}
	}
	l.buf.Write(p)
	return len(p), nil
}

// streamWriter forwards chunks to onOutput and keeps a copy (bounded)
// for error messages.
type streamWriter struct {
	mu   *sync.Mutex
	fn   func([]byte)
	copy *limitBuffer
}

func (s streamWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fn != nil {
		s.fn(append([]byte(nil), p...))
	}
	s.copy.Write(p)
	return len(p), nil
}

func gitEnv(read bool) []string {
	env := os.Environ()
	set := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"LC_ALL":              "C",
		"LANG":                "C",
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"GIT_MERGE_AUTOEDIT":  "no",
		"GIT_PAGER":           "cat",
		"PAGER":               "cat",
		"GCM_INTERACTIVE":     "never",
	}
	if read {
		set["GIT_OPTIONAL_LOCKS"] = "0"
	}
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, over := set[k]; over {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

// run runs `git -C dir args...`.
func run(ctx context.Context, dir string, o runOpts, args ...string) (result, error) {
	bin, err := gitBinary()
	if err != nil {
		return result{}, err
	}
	timeout := o.timeout
	if timeout == 0 {
		timeout = WriteTimeout
		if o.read {
			timeout = ReadTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pre := []string{"-C", dir, "-c", "core.quotePath=false", "-c", "color.ui=false"}
	if o.literal {
		pre = append(pre, "--literal-pathspecs")
	}
	full := append(pre, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	proc.HideConsole(cmd)
	cmd.Env = gitEnv(o.read)
	// A credential helper or ssh can outlive git holding the pipes; don't
	// wait for them forever once git itself is gone.
	cmd.WaitDelay = 3 * time.Second
	if o.stdin != "" {
		cmd.Stdin = strings.NewReader(o.stdin)
	}

	var res result
	stdout := &limitBuffer{max: o.limit}
	stderr := &limitBuffer{max: 64 << 10}
	if o.onOutput != nil {
		mu := &sync.Mutex{}
		both := &limitBuffer{max: 64 << 10}
		cmd.Stdout = io.MultiWriter(stdout, streamWriter{mu: mu, fn: o.onOutput, copy: both})
		cmd.Stderr = streamWriter{mu: mu, fn: o.onOutput, copy: stderr}
	} else {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}
	runErr := cmd.Run()
	res.stdout = stdout.buf.Bytes()
	res.stderr = stderr.buf.String()
	res.truncated = stdout.truncated
	if runErr == nil {
		return res, nil
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) {
		res.code = exit.ExitCode()
		for _, c := range o.okCodes {
			if c == res.code {
				return res, nil
			}
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return res, &Error{Args: args, Output: fmt.Sprintf("timed out after %s", timeout), Err: ctxErr}
		}
		return res, &Error{Args: args, Output: "cancelled", Err: ctxErr}
	}
	out := res.stderr
	if strings.TrimSpace(out) == "" {
		out = string(res.stdout)
	}
	return res, &Error{Args: args, Output: out, Err: runErr}
}

// output runs a read command and returns its stdout.
func output(ctx context.Context, dir string, args ...string) (string, error) {
	r, err := run(ctx, dir, runOpts{read: true}, args...)
	return string(r.stdout), err
}

// write runs a local write command, returning its combined output.
func write(ctx context.Context, dir string, args ...string) (string, error) {
	r, err := run(ctx, dir, runOpts{}, args...)
	return strings.TrimSpace(string(r.stdout) + "\n" + r.stderr), err
}

// writePaths is write for commands that take file paths (literal pathspecs).
func writePaths(ctx context.Context, dir string, args ...string) (string, error) {
	r, err := run(ctx, dir, runOpts{literal: true}, args...)
	return strings.TrimSpace(string(r.stdout) + "\n" + r.stderr), err
}

// network runs a remote operation, streaming its output, and turns an
// authentication failure into an AuthError.
func network(ctx context.Context, dir, op string, onOutput func([]byte), args ...string) error {
	r, err := run(ctx, dir, runOpts{timeout: NetworkTimeout, onOutput: onOutput}, args...)
	if err != nil {
		if isAuthFailure(r.stderr + string(r.stdout)) {
			return &AuthError{Op: op, Output: r.stderr}
		}
		return err
	}
	return nil
}

/* ── validation ──────────────────────────────────────────────── */

// ValidPath checks a repo-relative path from the page: not empty, not
// absolute, no "..", no NUL. Returns it cleaned, with forward slashes.
func ValidPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("no file given")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || filepath.VolumeName(p) != "" {
		return "", fmt.Errorf("%q is an absolute path — only files inside the project can be used", p)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if clean == "." {
		return "", errors.New("no file given")
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", fmt.Errorf("%q points outside the project", p)
		}
	}
	return clean, nil
}

func validPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("no files given")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		c, err := ValidPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// validRefArg rejects anything that could be read as an option or that
// isn't a plausible ref.
func validRefArg(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return errors.New("no branch given")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("%q isn't a valid branch name", ref)
	}
	if strings.ContainsAny(ref, " \t\r\n\x00~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return fmt.Errorf("%q isn't a valid branch name", ref)
	}
	return nil
}

// ValidBranchName checks a new branch name with git's own rules.
func ValidBranchName(ctx context.Context, dir, name string) error {
	if err := validRefArg(name); err != nil {
		return err
	}
	if _, err := run(ctx, dir, runOpts{read: true}, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("%q isn't a valid branch name", name)
	}
	return nil
}
