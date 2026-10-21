//go:build !windows

package selfupdate

import (
	"os/exec"
	"syscall"
)

// listProcesses finds the running Mullion pieces (and Mullion.app).
func listProcesses(self int) []Process {
	comm, err1 := exec.Command("ps", "-axo", "pid=,comm=").Output()
	args, err2 := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err1 != nil || err2 != nil {
		return nil
	}
	return parsePS(string(comm), string(args), self)
}

func terminate(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

func kill(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }

// alive reports whether pid still exists (a zombie counts as gone once
// its parent reaps it; detached helpers are reparented to launchd,
// which does so promptly).
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
