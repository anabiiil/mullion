package caddy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pm/internal/download"
	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/sysproc"
)

const adminEndpoint = "http://127.0.0.1:2019"

// EnsureInstalled downloads caddy into ~/.mullion/bin if it's missing.
func EnsureInstalled(ctx context.Context, paths pmdir.Paths) error {
	if _, err := os.Stat(paths.CaddyExe()); err == nil {
		return nil
	}
	fmt.Println("Downloading Caddy...")
	url := fmt.Sprintf("https://caddyserver.com/api/download?os=%s&arch=%s", runtime.GOOS, runtime.GOARCH)
	if err := download.ToFile(ctx, url, paths.CaddyExe()); err != nil {
		return err
	}
	// download.ToFile writes through a temp file that loses the exec bit.
	return os.Chmod(paths.CaddyExe(), 0o755)
}

// Running reports whether the Caddy admin API is answering.
func Running() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(adminEndpoint + "/config/")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ServingOurs reports whether the caddy answering on the admin port is
// actually running MULLION'S config — an unrelated caddy (or one from a
// stale install) would swallow every reload while serving old content.
func ServingOurs(paths pmdir.Paths) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(adminEndpoint + "/config/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false
	}
	// Our config always references log files under our logs dir
	// (JSON-escaped on Windows).
	marker := strings.ReplaceAll(paths.LogsDir(), `\`, `\\`)
	return strings.Contains(string(body), marker)
}

func pidFile(paths pmdir.Paths) string {
	return filepath.Join(paths.PidsDir(), "caddy.pid")
}

// Start launches Caddy in the background and waits for it to come up.
// It runs `caddy run` detached rather than `caddy start`: the child that
// `caddy start` spawns stays attached to the current console, so closing
// the terminal would silently kill the server.
func Start(paths pmdir.Paths) error {
	// Kill ZOMBIE caddy generations: a caddy process holding the web
	// ports that is NOT the one answering the admin port can never be
	// reloaded — it serves a stale config forever (the classic "my
	// changes don't show up"). Only processes named caddy are touched.
	adminPid := 0
	if Running() {
		adminPid, _ = sysproc.PortOwner(2019)
	}
	for _, p := range []int{80, 443} {
		pid, name := sysproc.PortOwner(p)
		if pid > 0 && pid != adminPid && strings.Contains(strings.ToLower(name), "caddy") {
			fmt.Printf("Removing a stale Caddy holding port %d (PID %d)...\n", p, pid)
			sysproc.KillWithParent(pid)
		}
	}

	if Running() {
		if ServingOurs(paths) {
			return Reload(paths)
		}
		// A caddy that is NOT ours holds the ports (an old install, a
		// manual instance): reloading it is useless and leaving it means
		// stale sites forever — replace it.
		fmt.Println("A Caddy that is not Mullion's is running — replacing it...")
		if pid, _ := sysproc.PortOwner(2019); pid > 0 {
			sysproc.KillWithParent(pid)
		}
		for i := 0; i < 20 && Running(); i++ {
			time.Sleep(250 * time.Millisecond)
		}
		killByPidFile(paths)
	}

	logPath := filepath.Join(paths.LogsDir(), "caddy.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := proc.Quiet(paths.CaddyExe(), "run",
		"--config", paths.Caddyfile(), "--adapter", "caddyfile")
	cmd.Dir = paths.Home
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting caddy: %w", err)
	}
	pid := cmd.Process.Pid
	// Detach: the process must outlive us.
	_ = cmd.Process.Release()

	if err := os.WriteFile(pidFile(paths), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}

	for i := 0; i < 50; i++ {
		if Running() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("caddy did not start (see %s)", logPath)
}

// Reload applies the current Caddyfile to the running instance.
func Reload(paths pmdir.Paths) error {
	if !Running() {
		return nil
	}
	if !ServingOurs(paths) {
		return nil // a foreign caddy is not ours to reload
	}
	// Port 2019 answering while our caddy was never downloaded means a
	// FOREIGN caddy owns the admin port — nothing of ours to reload.
	if _, err := os.Stat(paths.CaddyExe()); err != nil {
		return nil
	}
	cmd := proc.Quiet(paths.CaddyExe(), "reload",
		"--config", paths.Caddyfile(), "--adapter", "caddyfile")
	cmd.Dir = paths.Home
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy reload: %v: %s", err, out)
	}
	return nil
}

// Stop shuts down the running instance (no-op when not running).
func Stop(paths pmdir.Paths) error {
	defer os.Remove(pidFile(paths))
	if !Running() {
		killByPidFile(paths)
		return nil
	}
	// The admin port answering while our caddy was never downloaded
	// means a FOREIGN caddy owns it — `caddy stop` would shut THAT one
	// down. Only fall back to our own recorded pid (if any).
	if _, err := os.Stat(paths.CaddyExe()); err != nil {
		killByPidFile(paths)
		return nil
	}
	cmd := proc.Quiet(paths.CaddyExe(), "stop")
	cmd.Dir = paths.Home
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy stop: %v: %s", err, out)
	}
	return nil
}

// killByPidFile is the fallback for an instance whose admin API is not
// answering but whose process may still be alive.
func killByPidFile(paths pmdir.Paths) {
	data, err := os.ReadFile(pidFile(paths))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// TrustCA asks Caddy to install its local root CA into the system trust
// store so browsers accept the generated certificates. On Windows this
// triggers one UAC prompt; on macOS it runs sudo, so the terminal may
// ask for your password.
func TrustCA(paths pmdir.Paths) error {
	cmd := proc.Quiet(paths.CaddyExe(), "trust")
	cmd.Dir = paths.Home
	if runtime.GOOS != "windows" && stdinIsTerminal() {
		fmt.Println("Installing the local root certificate — you may be asked for your password.")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("caddy trust: %w", err)
		}
		return nil
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("caddy trust: %v: %s", err, out)
	}
	return nil
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
