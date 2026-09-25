//go:build windows

package workers

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"pm/internal/proc"
)

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// processAlive asks the kernel directly (no tasklist spawn — the
// supervisor checks every few seconds).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
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

// terminate kills the worker and its whole child tree (Windows has no
// SIGTERM for console-less processes; taskkill /T walks the tree).
func terminate(pid int, _ time.Duration) {
	_ = proc.Quiet("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// KillTree terminates a process and its child tree.
func KillTree(pid int) {
	if pid <= 0 {
		return
	}
	terminate(pid, 0)
}

func comspec() string {
	if c := os.Getenv("ComSpec"); c != "" {
		return c
	}
	return "cmd.exe"
}

// shellCmdLine passes the command to cmd.exe verbatim: Go's argv
// escaping would mangle quotes cmd does not understand. /S strips just
// the outer pair of quotes and keeps everything inside as typed.
func shellCmdLine(command string) string {
	return `cmd.exe /S /C "` + command + `"`
}

// detachedShell builds a worker's command: cmd /C with its own hidden
// console, independent of the CLI/panel that started it.
func detachedShell(command string) *exec.Cmd {
	cmd := exec.Command(comspec())
	proc.DetachHiddenConsole(cmd)
	cmd.SysProcAttr.CmdLine = shellCmdLine(command)
	return cmd
}

// ShellCommand builds a foreground shell command (no console window);
// KillTree(cmd.Process.Pid) takes down everything it spawned.
func ShellCommand(command string) *exec.Cmd {
	cmd := exec.Command(comspec())
	proc.HideConsole(cmd)
	cmd.SysProcAttr.CmdLine = shellCmdLine(command)
	return cmd
}

// InteractiveShellCommand builds a shell command meant to share the
// caller's console (visible, Ctrl+C reaches it).
func InteractiveShellCommand(command string) *exec.Cmd {
	cmd := exec.Command(comspec())
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: shellCmdLine(command)}
	return cmd
}
