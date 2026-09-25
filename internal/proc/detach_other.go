//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// Detach puts the process in its own session so it survives the CLI and
// its terminal going away.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// DetachHiddenConsole is identical to Detach off Windows.
func DetachHiddenConsole(cmd *exec.Cmd) { Detach(cmd) }

// HideConsole is a no-op off Windows.
func HideConsole(cmd *exec.Cmd) {}

// Quiet is plain exec.Command off Windows.
func Quiet(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
