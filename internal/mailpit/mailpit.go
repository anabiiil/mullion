// Package mailpit installs and runs Mailpit (github.com/axllent/mailpit,
// MIT) — a single-binary SMTP server that catches every message an app
// sends and shows it in a web inbox — under ~/.mullion/mailpit, the same
// way the database engines run: one binary per version, one detached
// process, a pid file, and a port probe to know whether it's up.
//
// Layout (under paths.Home/mailpit):
//
//	<version>/mailpit     the binary (mailpit.exe on Windows) + its LICENSE
//	mailpit.db            the mailbox (kept by `mullion mail uninstall --keep-mail`)
//
// It does not import app: app wires it into Apply, the ports, Caddy and
// php.ini (internal/app/mail.go).
package mailpit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"pm/internal/archive"
	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/sysproc"
)

// Default ports: Mailpit's own (and MailHog's) defaults, which framework
// docs and .env examples already use.
const (
	DefaultSMTPPort = 1025
	DefaultUIPort   = 8025
)

// SMTPPort and UIPort are where Mullion's Mailpit listens (127.0.0.1
// only): the defaults unless the user moved them (config mailSmtpPort /
// mailUiPort). app.ApplyPortConfig sets them at startup.
var (
	SMTPPort = DefaultSMTPPort
	UIPort   = DefaultUIPort
)

// MaxMessages caps the mailbox: Mailpit deletes the oldest messages
// beyond it, so a runaway job can't fill the disk.
const MaxMessages = 1000

// SiteName is the subdomain the inbox is served on: https://mail.<tld>.
const SiteName = "mail"

// Overridable for tests: where the GitHub API lives and which repo
// publishes Mailpit's releases.
var (
	APIBase = "https://api.github.com"
	Repo    = "axllent/mailpit"
)

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// Label renders a version for humans: "Mailpit 1.31.6".
func Label(version string) string { return "Mailpit " + version }

// BaseDir holds every installed version and the mailbox database.
func BaseDir(paths pmdir.Paths) string { return paths.MailpitDir() }

func versionDir(paths pmdir.Paths, version string) string {
	return filepath.Join(BaseDir(paths), version)
}

// Exe is the Mailpit binary of an installed version.
func Exe(paths pmdir.Paths, version string) string {
	return filepath.Join(versionDir(paths, version), pmdir.ExeName("mailpit"))
}

// DBFile is the mailbox: every caught message lives in this SQLite file.
func DBFile(paths pmdir.Paths) string { return filepath.Join(BaseDir(paths), "mailpit.db") }

// LogFile is where Mailpit's output goes.
func LogFile(paths pmdir.Paths) string { return filepath.Join(paths.LogsDir(), "mailpit.log") }

func pidFile(paths pmdir.Paths) string { return filepath.Join(paths.PidsDir(), "mailpit.pid") }

var fullVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Installed lists the versions present on disk, newest first.
func Installed(paths pmdir.Paths) []string {
	entries, err := os.ReadDir(BaseDir(paths))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !fullVersionRe.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(Exe(paths, e.Name())); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return Newer(out[i], out[j]) })
	return out
}

// Asset is one downloadable file of a GitHub release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is the subset of GitHub's release JSON Mullion needs.
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// Version is the release tag without its leading "v".
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// assetName is the release archive built for goos/goarch — Mailpit
// publishes mailpit-<os>-<arch>.tar.gz, and .zip for Windows (checked
// against the v1.31 release) — or "" when there is none.
func assetName(goos, goarch string) string {
	switch goos {
	case "darwin", "linux":
		if goarch == "amd64" || goarch == "arm64" {
			return "mailpit-" + goos + "-" + goarch + ".tar.gz"
		}
	case "windows":
		if goarch == "amd64" || goarch == "arm64" {
			return "mailpit-windows-" + goarch + ".zip"
		}
	}
	return ""
}

// pickAsset chooses the release archive for goos/goarch.
func pickAsset(assets []Asset, goos, goarch string) (Asset, error) {
	want := assetName(goos, goarch)
	if want == "" {
		return Asset{}, fmt.Errorf("Mailpit is not published for %s/%s", goos, goarch)
	}
	for _, a := range assets {
		if a.Name == want {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("the latest Mailpit release has no %s", want)
}

// Latest fetches the latest release's metadata from GitHub.
func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("checking for Mailpit releases: HTTP %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("reading the Mailpit release: %w", err)
	}
	if !fullVersionRe.MatchString(rel.Version()) {
		return Release{}, fmt.Errorf("unexpected Mailpit release tag %q", rel.Tag)
	}
	latestCache.Lock()
	latestCache.version, latestCache.at = rel.Version(), time.Now()
	latestCache.Unlock()
	return rel, nil
}

// latestCache keeps the last lookup for a while: the panel asks every
// time the Mail page opens, and the unauthenticated GitHub API allows
// only 60 requests an hour.
var latestCache struct {
	sync.Mutex
	version string
	at      time.Time
}

// LatestVersion is a cached, best-effort Latest().Version().
func LatestVersion(ctx context.Context) (string, error) {
	latestCache.Lock()
	if latestCache.version != "" && time.Since(latestCache.at) < 30*time.Minute {
		v := latestCache.version
		latestCache.Unlock()
		return v, nil
	}
	latestCache.Unlock()
	rel, err := Latest(ctx)
	if err != nil {
		return "", err
	}
	return rel.Version(), nil
}

// Progress reports download progress in bytes (total is 0 when unknown).
type Progress func(done, total int64)

// Install downloads the latest release for this platform into
// <base>/<version>/ and returns that version. An already-present
// version is not downloaded again.
func Install(ctx context.Context, paths pmdir.Paths, progress Progress) (string, error) {
	rel, err := Latest(ctx)
	if err != nil {
		return "", err
	}
	return installRelease(ctx, paths, rel, runtime.GOOS, runtime.GOARCH, progress)
}

func installRelease(ctx context.Context, paths pmdir.Paths, rel Release, goos, goarch string, progress Progress) (string, error) {
	if progress == nil {
		progress = func(int64, int64) {}
	}
	version := rel.Version()
	if _, err := os.Stat(Exe(paths, version)); err == nil {
		return version, nil
	}
	asset, err := pickAsset(rel.Assets, goos, goarch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(BaseDir(paths), 0o755); err != nil {
		return "", err
	}
	// Staged next to the final folder (same volume, so the rename is
	// atomic), and only renamed into place once the binary is there.
	stage, err := os.MkdirTemp(BaseDir(paths), ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, asset.Name)
	if err := fetch(ctx, asset.URL, file, progress); err != nil {
		return "", err
	}
	out := filepath.Join(stage, "out")
	if strings.HasSuffix(asset.Name, ".zip") {
		err = archive.ExtractZip(file, out)
	} else {
		err = archive.ExtractTarGz(file, out)
	}
	if err != nil {
		return "", fmt.Errorf("unpacking %s: %w", asset.Name, err)
	}
	bin := "mailpit"
	if goos == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(filepath.Join(out, bin)); err != nil {
		return "", fmt.Errorf("%s has no %s", asset.Name, bin)
	}
	if err := os.Chmod(filepath.Join(out, bin), 0o755); err != nil {
		return "", err
	}
	// Only the binary and its LICENSE (MIT asks for the notice to travel
	// with it); the README stays out.
	os.Remove(filepath.Join(out, "README.md"))
	dest := versionDir(paths, version)
	os.RemoveAll(dest)
	if err := os.Rename(out, dest); err != nil {
		return "", err
	}
	return version, nil
}

// fetch downloads url into dest, reporting progress about every 150ms.
func fetch(ctx context.Context, url, dest string, progress Progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading Mailpit: HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	pw := &progressWriter{total: resp.ContentLength, report: progress}
	_, err = io.Copy(io.MultiWriter(f, pw), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading Mailpit: %w", err)
	}
	progress(pw.done, pw.total)
	return nil
}

type progressWriter struct {
	done, total int64
	last        time.Time
	report      Progress
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if time.Since(w.last) > 150*time.Millisecond {
		w.last = time.Now()
		w.report(w.done, w.total)
	}
	return len(p), nil
}

// RemoveVersion deletes one installed version's folder (after an
// upgrade moved on from it).
func RemoveVersion(paths pmdir.Paths, version string) error {
	if !fullVersionRe.MatchString(version) {
		return fmt.Errorf("invalid Mailpit version %q", version)
	}
	return os.RemoveAll(versionDir(paths, version))
}

// Remove deletes every installed version, and the mailbox too unless
// keepMail. Stopping the server first is the caller's job.
func Remove(paths pmdir.Paths, keepMail bool) error {
	if !keepMail {
		return os.RemoveAll(BaseDir(paths))
	}
	entries, err := os.ReadDir(BaseDir(paths))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		// mailpit.db plus its SQLite side files (-wal, -shm) stay.
		if strings.HasPrefix(e.Name(), "mailpit.db") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(BaseDir(paths), e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Args is Mailpit's command line: loopback only on both ports, the
// mailbox in Mullion's folder, any SMTP login accepted (frameworks send
// MAIL_USERNAME/PASSWORD even when they are "null") over plain SMTP, a
// bounded mailbox, no phoning home for version checks — and the release
// relay when the user enabled one (relay.go).
func Args(paths pmdir.Paths) []string {
	args := []string{
		"--smtp", "127.0.0.1:" + strconv.Itoa(SMTPPort),
		"--listen", "127.0.0.1:" + strconv.Itoa(UIPort),
		"--database", DBFile(paths),
		"--smtp-auth-accept-any",
		"--smtp-auth-allow-insecure",
		"--max", strconv.Itoa(MaxMessages),
		"--disable-version-check",
	}
	return append(args, relayArgs(paths)...)
}

// SendmailCommand is the sendmail replacement PHP's mail() pipes
// messages into: Mailpit's own `sendmail` subcommand pointed at its
// SMTP port (it reads the recipients from the message headers; PHP's
// usual -t/-i flags are accepted and ignored). A path with spaces is
// single-quoted for the shell PHP runs it through.
func SendmailCommand(exe string, port int) string {
	if strings.ContainsAny(exe, " \t") {
		exe = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	}
	return exe + " sendmail -S 127.0.0.1:" + strconv.Itoa(port)
}

// portUp reports whether something accepts connections on the port.
func portUp(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Running reports whether something serves the inbox port.
func Running() bool { return portUp(UIPort) }

// SMTPUp reports whether something accepts connections on the SMTP port.
func SMTPUp() bool { return portUp(SMTPPort) }

// ourPids lists the Mailpit server Mullion started: a process running
// from Mullion's folder (never someone else's Mailpit, which must not be
// stopped) — the one in the pid file when that is still alive, so a
// short-lived `mailpit sendmail` that PHP spawned isn't taken for it.
var ourPids = func(paths pmdir.Paths) []int {
	pids := sysproc.ProcessesUnder(BaseDir(paths), pmdir.ExeName("mailpit"))
	if b, err := os.ReadFile(pidFile(paths)); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			for _, p := range pids {
				if p == pid {
					return []int{pid}
				}
			}
		}
	}
	return pids
}

// Owned reports whether Mullion's own Mailpit is running.
func Owned(paths pmdir.Paths) bool { return len(ourPids(paths)) > 0 }

// Start launches Mailpit detached and waits until both ports answer.
// A port already held by something that isn't Mullion's Mailpit (a
// Homebrew or Docker Mailpit/MailHog) is reported instead of fought.
func Start(paths pmdir.Paths, version string) error {
	if Owned(paths) {
		if Running() && SMTPUp() {
			return nil
		}
		// Ours, but not answering where it should (still on old ports,
		// or wedged): replace it.
		if err := Stop(paths); err != nil {
			return err
		}
	}
	exe := Exe(paths, version)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("%s is not installed (run: mullion mail install)", Label(version))
	}
	for _, p := range []int{SMTPPort, UIPort} {
		if portUp(p) {
			who := "another program"
			if pid, name := sysproc.PortOwner(p); pid > 0 {
				who = fmt.Sprintf("%s (pid %d)", name, pid)
			}
			return fmt.Errorf("port %d is already used by %s — stop it, or move Mail to other ports (Ports page, or `mullion ports`)", p, who)
		}
	}
	for _, dir := range []string{BaseDir(paths), paths.LogsDir(), paths.PidsDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	out, err := os.OpenFile(LogFile(paths), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := proc.Quiet(exe, Args(paths)...)
	cmd.Dir = BaseDir(paths)
	cmd.Stdout = out
	cmd.Stderr = out
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting Mailpit: %w", err)
	}
	if err := os.WriteFile(pidFile(paths), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for i := 0; i < 60; i++ {
		if Running() && SMTPUp() {
			return nil
		}
		select {
		case err := <-exited:
			os.Remove(pidFile(paths))
			return fmt.Errorf("Mailpit exited during startup (%v)%s — see %s", err, lastLogLine(paths), LogFile(paths))
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("Mailpit did not start listening on ports %d/%d (see %s)", SMTPPort, UIPort, LogFile(paths))
}

// lastLogLine is the newest error-ish line of Mailpit's log, for
// startup failures.
func lastLogLine(paths pmdir.Paths) string {
	data, err := os.ReadFile(LogFile(paths))
	if err != nil {
		return ""
	}
	if len(data) > 64<<10 {
		data = data[len(data)-64<<10:]
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-20; i-- {
		if strings.Contains(lines[i], "level=error") || strings.Contains(lines[i], "level=fatal") {
			return ": " + strings.TrimSpace(lines[i])
		}
	}
	return ""
}

// Stop shuts Mullion's Mailpit down (SIGTERM, then a hard kill) and
// waits for its ports to close. Mailpit commits every message as it
// arrives, so there is nothing to flush. Not running is not an error.
func Stop(paths pmdir.Paths) error {
	defer os.Remove(pidFile(paths))
	pids := ourPids(paths)
	if len(pids) == 0 {
		return nil
	}
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			if runtime.GOOS == "windows" || p.Signal(syscall.SIGTERM) != nil {
				_ = p.Kill()
			}
		}
	}
	if waitStopped(paths, 5*time.Second) {
		return nil
	}
	for _, pid := range ourPids(paths) {
		sysproc.KillProcess(pid)
	}
	if !waitStopped(paths, 5*time.Second) {
		return fmt.Errorf("Mailpit is still running")
	}
	return nil
}

func waitStopped(paths pmdir.Paths, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(ourPids(paths)) == 0 {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return len(ourPids(paths)) == 0
}

// Newer reports whether version a is newer than b ("1.31.10" > "1.31.9").
func Newer(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, s := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}

// ErrNotRunning is returned by the API client when the inbox port
// doesn't answer.
var ErrNotRunning = errors.New("Mailpit is not running")
