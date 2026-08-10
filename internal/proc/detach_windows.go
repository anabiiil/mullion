//go:build windows

// Package proc helps launch long-lived background processes that must
// outlive the mullion CLI and the terminal it was run from.
package proc

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// Detach makes the process survive the CLI exiting and the terminal
// closing (no console attachment), without flashing a console window.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}

// DetachHiddenConsole gives the process its own INVISIBLE console
// instead of none at all. Console hosts like powershell.exe misbehave
// under DETACHED_PROCESS; with CREATE_NO_WINDOW they run normally,
// independent of the parent's console lifetime.
func DetachHiddenConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | createNoWindow,
	}
}
