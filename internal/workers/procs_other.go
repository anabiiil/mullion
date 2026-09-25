//go:build !windows

package workers

import (
	"os/exec"
	"syscall"
	"time"

	"pm/internal/proc"
)

// processAlive reports whether pid is one of OUR workers still running.
// Workers are session leaders (proc.Detach → Setsid), so their pgid is
// their own pid; checking that too keeps a recycled pid (some unrelated
// process that inherited the number) from looking like a live worker.
func processAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	pgid, err := syscall.Getpgid(pid)
	return err == nil && pgid == pid
}

// groupAlive is processAlive without the session-leader requirement,
// for the plain process groups ShellCommand creates (Setpgid).
func groupAlive(pid int) bool {
	return pid > 0 && syscall.Kill(-pid, 0) == nil
}

// terminate stops a worker's whole process group: SIGTERM first (queue
// workers finish the job in hand and exit cleanly), SIGKILL for
// whatever is still there after the grace period.
func terminate(pid int, grace time.Duration) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !groupAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// KillTree terminates a process group started by ShellCommand (SIGTERM,
// then SIGKILL after a few seconds, without blocking the caller).
func KillTree(pid int) {
	if pid <= 0 {
		return
	}
	go terminate(pid, 5*time.Second)
}

// detachedShell builds a worker's command: /bin/sh -c in its own
// session, so it outlives the CLI/panel that started it.
func detachedShell(command string) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", command)
	proc.Detach(cmd)
	return cmd
}

// ShellCommand builds a foreground (not detached) shell command in its
// own process group, so KillTree(cmd.Process.Pid) takes down everything
// it spawned.
func ShellCommand(command string) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// InteractiveShellCommand builds a shell command meant to share the
// caller's terminal: same process group, so Ctrl+C reaches it.
func InteractiveShellCommand(command string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", command)
}
