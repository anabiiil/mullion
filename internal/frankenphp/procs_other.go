//go:build !windows

package frankenphp

import (
	"strconv"
	"strings"
	"syscall"

	"pm/internal/proc"
)

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// killTree terminates the process group (proc.DetachHiddenConsole uses
// Setsid, so the group leader's pid doubles as the pgid — this takes the
// whole frankenphp, or php-cli artisan → frankenphp, tree down).
func killTree(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// forceKillTree SIGKILLs the process group: the last resort when it
// ignored killTree's SIGTERM.
func forceKillTree(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// processName is the command name of pid (`ps -o comm=`, the executable
// path on macOS), or "" when it can't be read.
func processName(pid int) string {
	out, err := proc.Quiet("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
