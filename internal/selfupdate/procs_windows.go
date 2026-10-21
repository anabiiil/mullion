//go:build windows

package selfupdate

import (
	"fmt"
	"syscall"

	"pm/internal/proc"
)

// listProcesses finds the running Mullion pieces — by image name, with
// their command lines, which only WMI exposes.
func listProcesses(self int) []Process {
	out, err := proc.Quiet("powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_Process -Filter "Name LIKE 'mullion%.exe'" | ForEach-Object { "$($_.ProcessId)`+"`t"+`$($_.ExecutablePath)`+"`t"+`$($_.CommandLine)" }`).Output()
	if err != nil {
		return nil
	}
	return parseWinProcs(string(out), self)
}

// terminate and kill are the same on Windows: no polite signal reaches
// a windowless process. Never /T: the tray's child processes include
// the Caddy, PHP and MySQL servers it started.
func terminate(pid int) { kill(pid) }

func kill(pid int) { _ = proc.Quiet("taskkill", "/F", "/PID", fmt.Sprint(pid)).Run() }

func alive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	const stillActive = 259
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
