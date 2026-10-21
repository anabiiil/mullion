package selfupdate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"pm/internal/proc"
	"pm/internal/version"
)

// Reporter receives an upgrade's progress: Line gets each line of the
// package manager's output (or a step of a direct download), Status a
// short one-line summary of the current step. Either may be nil.
type Reporter struct {
	Line   func(string)
	Status func(string)
}

func (r Reporter) line(s string) {
	if r.Line != nil {
		r.Line(s)
	}
}

func (r Reporter) status(s string) {
	if r.Status != nil {
		r.Status(s)
	}
	r.line("==> " + s)
}

// Result describes a finished upgrade.
type Result struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Channel Channel `json:"channel"`
	// Exe is the new binary — the one to run `upgrade --finish` with.
	Exe string `json:"exe"`
}

// ErrUpToDate means there was nothing to upgrade to.
var ErrUpToDate = errors.New("Mullion is already up to date")

// Error is an upgrade failure that knows how to finish the job by hand.
type Error struct {
	Msg    string
	Manual string
}

func (e *Error) Error() string {
	if e.Manual == "" {
		return e.Msg
	}
	return e.Msg + " — to upgrade by hand, run: " + e.Manual
}

func manualErr(ch Channel, format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Manual: ch.ManualCommand()}
}

// Upgrade installs the latest release through this install's channel.
// It only puts the new binary on disk: running pieces keep the old code
// until the caller runs `<Result.Exe> upgrade --finish` (Relaunch).
func Upgrade(ctx context.Context, rep Reporter) (Result, error) {
	ch := DetectChannel()
	rep.status("Checking the latest Mullion release…")
	rel, err := LatestFresh(ctx)
	if err != nil {
		return Result{}, manualErr(ch, "%v", err)
	}
	if !Newer(rel.Version(), version.Number) {
		return Result{}, ErrUpToDate
	}
	res := Result{From: version.Number, To: rel.Version(), Channel: ch}
	switch ch {
	case Homebrew:
		res.Exe, err = upgradeBrew(ctx, rep)
	case Scoop:
		res.Exe, err = upgradeScoop(ctx, rep)
	default:
		res.Exe, err = upgradeDirect(ctx, rel, rep)
	}
	if err != nil {
		return res, err
	}
	got, verr := binaryVersion(ctx, res.Exe)
	if verr != nil {
		return res, manualErr(ch, "the upgrade finished but %s does not run: %v", res.Exe, verr)
	}
	if Newer(rel.Version(), got) {
		return res, manualErr(ch, "the upgrade finished but %s is still v%s", res.Exe, got)
	}
	res.To = got
	rep.status("Mullion v" + got + " is installed.")
	return res, nil
}

// ── Homebrew ──────────────────────────────────────────────────────

// brewCandidates are the brew executables tried before PATH.
var brewCandidates = []string{"/opt/homebrew/bin/brew", "/usr/local/bin/brew", "/home/linuxbrew/.linuxbrew/bin/brew"}

// lookPath is exec.LookPath, swappable in tests.
var lookPath = exec.LookPath

// geteuid is os.Geteuid, swappable in tests.
var geteuid = os.Geteuid

func findBrew() (string, bool) {
	for _, c := range brewCandidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, true
		}
	}
	if p, err := lookPath("brew"); err == nil {
		return p, true
	}
	return "", false
}

// brewSteps are the commands an upgrade runs, in order.
func brewSteps() [][]string {
	return [][]string{{"update"}, {"upgrade", BrewFormula}}
}

func upgradeBrew(ctx context.Context, rep Reporter) (string, error) {
	brew, ok := findBrew()
	if !ok {
		return "", manualErr(Homebrew, "Homebrew's brew command was not found (looked in /opt/homebrew/bin, /usr/local/bin and PATH)")
	}
	if geteuid() == 0 {
		return "", manualErr(Homebrew, "Homebrew must not run as root — run the upgrade as your own user")
	}
	env := brewEnv(os.Environ(), filepath.Dir(brew))
	steps := brewSteps()
	rep.status("Updating Homebrew…")
	if err := runStream(ctx, brew, steps[0], env, rep.line); err != nil {
		// A failed `brew update` (offline mirror, a broken unrelated
		// tap) shouldn't block the upgrade — brew may still know the
		// new formula. The upgrade step reports real failures.
		rep.line("note: brew update failed (" + err.Error() + ") — trying the upgrade anyway")
	}
	rep.status("Upgrading mullion with Homebrew…")
	if err := runStream(ctx, brew, steps[1], env, rep.line); err != nil {
		return "", manualErr(Homebrew, "brew upgrade failed: %v", err)
	}
	return filepath.Join(filepath.Dir(brew), "mullion"), nil
}

// brewEnv is the environment brew runs with: the panel may have been
// started from Finder with a bare PATH, and colors would litter the log.
func brewEnv(base []string, brewDir string) []string {
	var env []string
	old := ""
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			old = v
			continue
		}
		if strings.HasPrefix(kv, "HOMEBREW_NO_INSTALL_CLEANUP=") {
			continue
		}
		env = append(env, kv)
	}
	path := brewDir
	if old != "" {
		path += ":" + old
	}
	path += ":/usr/bin:/bin:/usr/sbin:/sbin"
	return append(env, "PATH="+path, "HOMEBREW_NO_COLOR=1", "HOMEBREW_NO_ENV_HINTS=1", "NONINTERACTIVE=1")
}

// ── Scoop ─────────────────────────────────────────────────────────

// findScoop locates Scoop's PowerShell entry point.
func findScoop() (string, bool) {
	for _, root := range scoopRoots() {
		p := filepath.Join(root, "shims", "scoop.ps1")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	if p, err := lookPath("scoop"); err == nil {
		return p, true
	}
	return "", false
}

// scoopCommand builds the PowerShell call that runs `scoop update` then
// `scoop update mullion`. Scoop refuses to update an app while any of
// its processes run — and Mullion's panel, tray and agent are running
// (the panel is the one asking). Old processes are retired by the
// relaunch right after, so the script turns Scoop's
// ignore_running_processes on for this one update and restores the
// user's previous setting afterwards, even on failure.
func scoopCommand(scoop string) (string, []string) {
	q := "'" + strings.ReplaceAll(scoop, "'", "''") + "'"
	script := strings.Join([]string{
		"$ErrorActionPreference = 'Continue'",
		"$scoop = " + q,
		"$prev = \"$(& $scoop config ignore_running_processes 2>$null)\"",
		"& $scoop config ignore_running_processes $true | Out-Null",
		"$code = 0",
		"try {",
		"  & $scoop update",
		"  & $scoop update mullion",
		"  if ($LASTEXITCODE) { $code = $LASTEXITCODE }",
		"} finally {",
		"  if ($prev -notmatch 'true') { & $scoop config rm ignore_running_processes | Out-Null }",
		"}",
		"exit $code",
	}, "\n")
	return "powershell", []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}
}

func upgradeScoop(ctx context.Context, rep Reporter) (string, error) {
	scoop, ok := findScoop()
	if !ok {
		return "", manualErr(Scoop, "Scoop was not found (looked in ~/scoop, $SCOOP and PATH)")
	}
	rep.status("Upgrading mullion with Scoop…")
	name, args := scoopCommand(scoop)
	if err := runStream(ctx, name, args, nil, rep.line); err != nil {
		return "", manualErr(Scoop, "scoop update failed: %v", err)
	}
	exe, ok := scoopInstalledMullion()
	if !ok {
		return "", manualErr(Scoop, "Scoop finished but its mullion.exe was not found")
	}
	return exe, nil
}

// ── helpers ───────────────────────────────────────────────────────

// runStream runs a command (no console window on Windows, stdin from
// the null device), handing each output line — stdout and stderr
// merged — to line as it arrives. env nil means inherit.
func runStream(ctx context.Context, name string, args []string, env []string, line func(string)) error {
	cmd := proc.Quiet(name, args...)
	if env != nil {
		cmd.Env = env
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	// A grandchild (brew's git, scoop's downloads) may keep the output
	// pipe open after a cancel kills the command; don't wait on it.
	cmd.WaitDelay = 3 * time.Second
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if line != nil {
				line(strings.TrimRight(sc.Text(), "\r"))
			}
		}
		io.Copy(io.Discard, pr)
	}()
	if err := cmd.Start(); err != nil {
		pw.Close()
		wg.Wait()
		return err
	}
	stop := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	err := cmd.Wait()
	stop()
	pw.Close()
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

var versionLine = regexp.MustCompile(`version\s+v?([0-9][0-9A-Za-z.+-]*)`)

// binaryVersion asks a mullion binary for its version (`--version`).
// MULLION_NO_REDIRECT keeps a ~/.mullion/bin copy from handing off.
var binaryVersion = func(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	proc.HideConsole(cmd)
	cmd.Env = append(os.Environ(), "MULLION_NO_REDIRECT=1")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return parseVersionOutput(string(out))
}

func parseVersionOutput(out string) (string, error) {
	m := versionLine.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected --version output %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

// exeName is the platform's file name for a program.
func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
