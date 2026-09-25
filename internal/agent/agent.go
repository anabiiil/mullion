// Package agent manages Mullion's tiny background helper: a local HTTP
// listener Caddy proxies to when a frontend site's dev server is down.
// It serves a "starting…" page and wakes the dev server — opening a
// link is all the user ever does.
package agent

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
	"pm/internal/sysproc"
	"pm/internal/version"
)

// Port is fixed and loopback-only; the dev-server ports assigned to
// sites start at 42001 and count up, far below this.
const Port = 42999

// WakeHeader carries the site to wake, set by Caddy's proxy config.
const WakeHeader = "X-Mullion-Wake"

func pidFile(paths pmdir.Paths) string {
	return filepath.Join(paths.PidsDir(), "agent.pid")
}

// Running reports whether the agent is listening.
func Running() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", Port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Ensure spawns the detached agent process when it isn't running — and
// replaces a running agent from an OLDER build, so upgrades take effect.
func Ensure(paths pmdir.Paths) error {
	if Running() {
		if runningVersion() == version.Number {
			return nil
		}
		// A stale agent (older build, or one whose pid record is gone)
		// must actually die — kill the PORT OWNER, not just the pid
		// file, and never report success while it still answers.
		Stop(paths)
		if pid, name := sysproc.PortOwner(Port); pid > 0 &&
			strings.Contains(strings.ToLower(name), "mullion") {
			sysproc.KillProcess(pid)
		}
		for i := 0; i < 30 && Running(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if Running() {
			pid, name := sysproc.PortOwner(Port)
			return fmt.Errorf("a stale process (%s, PID %d) is holding the agent port %d and could not be stopped", name, pid, Port)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(paths.LogsDir(), "agent.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := proc.Quiet(exe, "agent")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the mullion agent: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	if err := os.WriteFile(pidFile(paths), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		if Running() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the mullion agent did not start (see %s)", filepath.Join(paths.LogsDir(), "agent.log"))
}

// Stop kills the agent (no-op when not running).
func Stop(paths pmdir.Paths) {
	if data, err := os.ReadFile(pidFile(paths)); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	}
	_ = os.Remove(pidFile(paths))
}

// runningVersion asks the live agent which build it is.
func runningVersion() string {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/__mullion-agent-version", Port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
