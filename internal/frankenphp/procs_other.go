//go:build !windows

package frankenphp

import "syscall"

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
