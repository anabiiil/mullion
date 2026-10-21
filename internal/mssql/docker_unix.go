//go:build !windows

package mssql

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// dockerOverride replaces the docker CLI lookup (tests point it at a
// fake script).
var dockerOverride string

// homeDir is the user's home (a seam for tests).
var homeDir = func() string {
	h, _ := os.UserHomeDir()
	return h
}

func dockerPath() string {
	if dockerOverride != "" {
		return dockerOverride
	}
	return findDocker(exec.LookPath, fileExists, homeDir())
}

// docker runs one docker CLI command and returns its combined output.
// stdin may be nil.
func docker(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	bin := dockerPath()
	if bin == "" {
		return "", ErrNoDocker
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = dockerEnv(os.Environ(), homeDir(), Password, fileExists)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// dockerStream runs a docker command with its output on our stdout (the
// panel's job log / the terminal) — for the long image pull.
func dockerStream(ctx context.Context, args ...string) error {
	bin := dockerPath()
	if bin == "" {
		return ErrNoDocker
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = dockerEnv(os.Environ(), homeDir(), Password, fileExists)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", args[0], err)
	}
	return nil
}

// ── runtime (OrbStack / docker daemon) ────────────────────────────

// OrbStackInstalled reports whether OrbStack.app is present.
func OrbStackInstalled() bool {
	for _, p := range orbStackApps(homeDir()) {
		if fileExists(p) {
			return true
		}
	}
	return false
}

// daemonSocketUp is a cheap daemon check for status polls: dial the
// docker socket instead of running `docker info`.
func daemonSocketUp() bool {
	socks := []string{filepath.Join(homeDir(), ".orbstack", "run", "docker.sock"), "/var/run/docker.sock"}
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		socks = append([]string{strings.TrimPrefix(h, "unix://")}, socks...)
	}
	for _, s := range socks {
		if c, err := net.DialTimeout("unix", s, 300*time.Millisecond); err == nil {
			c.Close()
			return true
		}
	}
	return false
}

// QuickStatus reports the runtime state without running any docker
// command (file checks and a socket dial) — for the panel's poll.
// Container is not filled in.
func QuickStatus(paths pmdir.Paths) RuntimeStatus {
	st := RuntimeStatus{Kind: "container", OrbStack: OrbStackInstalled(), Docker: dockerPath() != ""}
	st.Daemon = st.Docker && daemonSocketUp()
	return st
}

// Status is QuickStatus plus whether Mullion's container exists (one
// `docker container inspect` when the daemon is up).
func Status(paths pmdir.Paths) RuntimeStatus {
	st := QuickStatus(paths)
	if st.Daemon {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := docker(ctx, nil, "container", "inspect", "--format", "{{.Id}}", ContainerName)
		st.Container = err == nil
	}
	return st
}

// daemonUp runs `docker info`, the authoritative "daemon answers" check.
func daemonUp(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := docker(c, nil, "info", "--format", "{{.ServerVersion}}")
	return err == nil
}

// startOrbStack launches OrbStack in the background: `orb start` when
// its CLI exists, else `open -ga OrbStack` (-g: don't steal focus).
var startOrbStack = func() error {
	orb := filepath.Join(homeDir(), ".orbstack", "bin", "orb")
	if p, err := exec.LookPath("orb"); err == nil {
		orb = p
	}
	if fileExists(orb) {
		if err := exec.Command(orb, "start").Run(); err == nil {
			return nil
		}
	}
	return exec.Command("open", "-ga", "OrbStack").Run()
}

// EnsureDaemon makes sure the docker daemon answers, starting OrbStack
// when it is installed but not running, and waiting up to timeout.
func EnsureDaemon(ctx context.Context, timeout time.Duration) error {
	if dockerPath() == "" {
		return ErrNoDocker
	}
	if daemonUp(ctx) {
		return nil
	}
	if !OrbStackInstalled() {
		return fmt.Errorf("the docker daemon is not running — start your Docker runtime (Mullion supports OrbStack)")
	}
	fmt.Println("Starting OrbStack…")
	if err := startOrbStack(); err != nil {
		return fmt.Errorf("starting OrbStack: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if daemonUp(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("OrbStack did not come up within %s. If this is its first launch, finish its setup window (it asks to approve its helper), then try again", timeout)
}

// ── container lifecycle ───────────────────────────────────────────

func containerExists(ctx context.Context) bool {
	_, err := docker(ctx, nil, "container", "inspect", "--format", "{{.Id}}", ContainerName)
	return err == nil
}

func containerRunning(ctx context.Context) bool {
	out, err := docker(ctx, nil, "container", "inspect", "--format", "{{.State.Running}}", ContainerName)
	return err == nil && strings.TrimSpace(out) == "true"
}

// createContainer runs a fresh container over the (kept) data volume.
func createContainer(ctx context.Context) error {
	if _, err := docker(ctx, nil, "volume", "create", VolumeName); err != nil {
		return err
	}
	_, err := docker(ctx, nil, runArgs(Port, Image)...)
	return err
}

// Install pulls the image, creates the data volume and the container,
// and waits until the server answers. password is the sa password the
// first start sets (an existing data volume keeps the password it was
// created with — callers keep that password across uninstalls).
func Install(ctx context.Context, paths pmdir.Paths, password string, progress func(string)) error {
	if err := ValidPassword(password); err != nil {
		return err
	}
	Password = password
	if progress == nil {
		progress = func(string) {}
	}
	progress("Checking OrbStack…")
	if err := EnsureDaemon(ctx, 90*time.Second); err != nil {
		return err
	}
	progress("Downloading the SQL Server 2025 image (about 1.5 GB, first time only)…")
	if err := dockerStream(ctx, "pull", "--platform", "linux/amd64", Image); err != nil {
		return err
	}
	if containerExists(ctx) {
		progress("Replacing the old SQL Server container (data is kept)…")
		if _, err := docker(ctx, nil, "rm", "-f", ContainerName); err != nil {
			return err
		}
	}
	progress("Creating the SQL Server container…")
	if err := createContainer(ctx); err != nil {
		return err
	}
	progress("Waiting for SQL Server to finish its first start (a minute or two under Rosetta)…")
	return waitReady(paths, 4*time.Minute)
}

// Uninstall removes the container; deleteData also removes the data
// volume and the image.
func Uninstall(ctx context.Context, paths pmdir.Paths, deleteData bool) error {
	if dockerPath() == "" {
		return nil // nothing could have been installed
	}
	if err := EnsureDaemon(ctx, 60*time.Second); err != nil {
		return err
	}
	if containerExists(ctx) {
		if _, err := docker(ctx, nil, "rm", "-f", ContainerName); err != nil {
			return err
		}
	}
	if deleteData {
		if _, err := docker(ctx, nil, "volume", "rm", "-f", VolumeName); err != nil {
			return err
		}
		_, _ = docker(ctx, nil, "image", "rm", Image)
		os.RemoveAll(paths.MssqlDir())
	}
	return nil
}

// Start brings the container up (starting OrbStack if needed, and
// recreating the container over its volume if it was removed) and
// waits until SQL Server answers.
func Start(paths pmdir.Paths) error {
	if err := Ensure(paths); err != nil {
		return err
	}
	return waitReady(paths, 3*time.Minute)
}

// Ensure is Start without waiting for SQL Server to be ready — what
// Apply uses, so a `mullion start` never blocks on the container's
// boot.
func Ensure(paths pmdir.Paths) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := EnsureDaemon(ctx, 90*time.Second); err != nil {
		return err
	}
	if !containerExists(ctx) {
		return createContainer(ctx)
	}
	if containerRunning(ctx) {
		return nil
	}
	_, err := docker(ctx, nil, "start", ContainerName)
	return err
}

// Stop stops the container (a no-op when the daemon or container is
// gone — nothing is running then).
func Stop(paths pmdir.Paths) error {
	if dockerPath() == "" || !daemonSocketUp() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if !containerRunning(ctx) {
		return nil
	}
	_, err := docker(ctx, nil, "stop", "-t", "30", ContainerName)
	return err
}

// StopFast is Stop for a whole-stack `mullion stop`: a short shutdown
// grace (10s) and a bounded wait, so the command never hangs on the
// container runtime.
func StopFast(paths pmdir.Paths) error {
	if dockerPath() == "" || !daemonSocketUp() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if !containerRunning(ctx) {
		return nil
	}
	_, err := docker(ctx, nil, "stop", "-t", "10", ContainerName)
	return err
}

// Owned reports whether Mullion's container is what serves the port.
func Owned() bool {
	if dockerPath() == "" || !daemonSocketUp() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return containerRunning(ctx)
}

// ApplyPort makes the container publish the current Port. A published
// port is fixed when a container is created, so a changed port means
// recreating it — the data volume is untouched. The container is left
// stopped if it was (the next Start creates it).
func ApplyPort(paths pmdir.Paths) error {
	if dockerPath() == "" || !daemonSocketUp() {
		return nil // recreated with the new port on the next Start
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !containerExists(ctx) {
		return nil
	}
	out, _ := docker(ctx, nil, "port", ContainerName, "1433/tcp")
	if publishedPort(out) == Port {
		return nil
	}
	running := containerRunning(ctx)
	if _, err := docker(ctx, nil, "rm", "-f", ContainerName); err != nil {
		return err
	}
	if running {
		return createContainer(ctx)
	}
	return nil
}

// ── queries ───────────────────────────────────────────────────────

func query(paths pmdir.Paths, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return runSQL(ctx, sql)
}

func queryQuiet(paths pmdir.Paths, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return runSQL(ctx, sql)
}

func runSQL(ctx context.Context, sql string) (string, error) {
	if Password == "" {
		return "", fmt.Errorf("no sa password is configured for SQL Server")
	}
	out, err := docker(ctx, strings.NewReader(sql+"\n"), sqlcmdArgs()...)
	if err != nil {
		return "", fmt.Errorf("SQL Server: %s", cleanSqlcmdError(out, err))
	}
	return out, nil
}

// cleanSqlcmdError picks the useful line out of a failed sqlcmd run.
func cleanSqlcmdError(out string, err error) string {
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "Msg ") || strings.HasPrefix(l, "Sqlcmd: Error") {
			continue
		}
		if l != "" {
			return l
		}
	}
	return err.Error()
}

// ── backup staging (inside the container) ─────────────────────────

func serverSep() string { return "/" }

func stagePath(paths pmdir.Paths, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := docker(ctx, nil, "exec", ContainerName, "mkdir", "-p", containerStageDir); err != nil {
		return "", err
	}
	return path.Join(containerStageDir, name), nil
}

func copyOut(paths pmdir.Paths, serverPath, local string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := docker(ctx, nil, "cp", ContainerName+":"+serverPath, local); err != nil {
		return err
	}
	_, _ = docker(ctx, nil, "exec", ContainerName, "rm", "-f", serverPath)
	return nil
}

func copyIn(paths pmdir.Paths, local string) (string, error) {
	serverPath, err := stagePath(paths, "restore-"+sanitizeFile(DatabaseFromBackupFile(local))+".bak")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if _, err := docker(ctx, nil, "cp", local, ContainerName+":"+serverPath); err != nil {
		return "", err
	}
	// docker cp writes the file as root; let the mssql user read it.
	_, _ = docker(ctx, nil, "exec", "-u", "0", ContainerName, "chmod", "644", serverPath)
	return serverPath, nil
}

func removeStaged(paths pmdir.Paths, serverPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = docker(ctx, nil, "exec", "-u", "0", ContainerName, "rm", "-f", serverPath)
}

// ── OrbStack install ──────────────────────────────────────────────

// orbStackDownloadURL is a seam for tests (an httptest server).
var orbStackDownloadURL = func() string { return OrbStackDownloadURL(runtime.GOARCH) }

// InstallOrbStack downloads OrbStack's official .dmg, copies the app
// into /Applications (falling back to ~/Applications when that is not
// writable), and launches it. Only ever run on the user's explicit
// request. OrbStack's first launch shows its own setup window, which
// may ask to approve its privileged helper — the caller should say so.
func InstallOrbStack(ctx context.Context, paths pmdir.Paths, progress func(string)) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("OrbStack is a macOS app")
	}
	if progress == nil {
		progress = func(string) {}
	}
	if OrbStackInstalled() {
		progress("OrbStack is already installed — starting it…")
		return startOrbStack()
	}
	tmp, err := os.MkdirTemp(paths.TmpDir(), "orbstack-")
	if err != nil {
		if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
			return err
		}
		if tmp, err = os.MkdirTemp(paths.TmpDir(), "orbstack-"); err != nil {
			return err
		}
	}
	defer os.RemoveAll(tmp)
	dmg := filepath.Join(tmp, "OrbStack.dmg")
	progress("Downloading OrbStack…")
	if err := downloadFile(ctx, orbStackDownloadURL(), dmg); err != nil {
		return err
	}
	mount := filepath.Join(tmp, "mnt")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return err
	}
	progress("Installing OrbStack…")
	if out, err := exec.CommandContext(ctx, "hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mount, dmg).CombinedOutput(); err != nil {
		return fmt.Errorf("mounting the OrbStack disk image: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer exec.Command("hdiutil", "detach", "-force", mount).Run()
	src := filepath.Join(mount, "OrbStack.app")
	if !fileExists(src) {
		return fmt.Errorf("the OrbStack disk image has no OrbStack.app")
	}
	dest := orbStackApps(homeDir())[0]
	if out, err := exec.CommandContext(ctx, "ditto", src, dest).CombinedOutput(); err != nil {
		// /Applications not writable (a standard account): use ~/Applications.
		dest = orbStackApps(homeDir())[1]
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if out2, err2 := exec.CommandContext(ctx, "ditto", src, dest).CombinedOutput(); err2 != nil {
			return fmt.Errorf("copying OrbStack.app: %v: %s / %s", err2, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	progress("Starting OrbStack — if it asks to approve its helper, accept it…")
	if out, err := exec.Command("open", dest).CombinedOutput(); err != nil {
		return fmt.Errorf("opening OrbStack: %v: %s", err, strings.TrimSpace(string(out)))
	}
	progress("Waiting for OrbStack's Docker engine (finish OrbStack's setup window if it shows one)…")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if dockerPath() != "" && daemonUp(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("OrbStack is installed, but its Docker engine did not answer within 5 minutes — finish OrbStack's setup window, then try again")
}
