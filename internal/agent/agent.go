// Package agent manages Mullion's tiny background helper: a local HTTP
// listener Caddy proxies to when a frontend site's dev server is down.
// It serves a "starting…" page and wakes the dev server — opening a
// link is all the user ever does.
package agent

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/proc"
	"pm/internal/pmdir"
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

// Ensure spawns the detached agent process when it isn't running.
func Ensure(paths pmdir.Paths) error {
	if Running() {
		return nil
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
