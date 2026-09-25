//go:build windows

// Package elevate detects and acquires administrator rights, so setup
// can ask for one UAC approval up front instead of one per step.
package elevate

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

// IsElevated reports whether the process runs with administrator rights.
func IsElevated() bool {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(p, syscall.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()

	const tokenElevation = 20
	var elevation, retLen uint32
	if err := syscall.GetTokenInformation(token, tokenElevation,
		(*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &retLen); err != nil {
		return false
	}
	return elevation != 0
}

// Relaunch starts exe with the given args through UAC (verb "runas"),
// which opens a new console window, waits for it to finish, and
// propagates its exit code — a failed elevated run must not look like
// success to the caller.
func Relaunch(exe string, args ...string) error {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = psQuote(a)
	}
	ps := fmt.Sprintf("$p = Start-Process -Verb RunAs -Wait -PassThru -FilePath %s -ArgumentList %s; exit $p.ExitCode",
		psQuote(exe), strings.Join(quoted, ","))
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("elevated run did not succeed (UAC declined, or the step in the new window failed): %v: %s", err, out)
	}
	return nil
}

// RelaunchAsync starts exe elevated WITHOUT waiting for it. Uninstall
// needs this: its elevated child kills every Mullion process, and a
// parent still waiting would either get killed mid-wait or keep
// bin\mullion.exe locked so the folder can never delete itself.
func RelaunchAsync(exe string, args ...string) error {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = psQuote(a)
	}
	ps := fmt.Sprintf("Start-Process -Verb RunAs -FilePath %s -ArgumentList %s",
		psQuote(exe), strings.Join(quoted, ","))
	cmd := exec.Command("powershell", "-NoProfile", "-Command", ps)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("elevation failed (UAC declined?): %v: %s", err, out)
	}
	return nil
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
