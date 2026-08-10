//go:build windows

// Package console detects how the process was launched so double-clicking
// mullion.exe in Explorer can do something useful instead of flashing a window.
package console

import (
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	getConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	getConsoleMode        = kernel32.NewProc("GetConsoleMode")
	getConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	flushConsoleInput     = kernel32.NewProc("FlushConsoleInputBuffer")
	user32                = syscall.NewLazyDLL("user32.dll")
	showWindow            = user32.NewProc("ShowWindow")
)

// FlushInput drops queued keystrokes. Call it right before asking a
// question: a stray Enter pressed during a long download would otherwise
// sit in the console buffer and silently "answer" the prompt.
func FlushInput() {
	if h, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE); err == nil {
		flushConsoleInput.Call(uintptr(h))
	}
}

// HideWindow hides this process's console window — used when the panel
// opens from a double-click, so only the panel window is visible.
func HideWindow() {
	if h, _, _ := getConsoleWindow.Call(); h != 0 {
		const swHide = 0
		showWindow.Call(h, swHide)
	}
}

// ShowWindow brings a hidden console back — for error messages that
// would otherwise be invisible.
func ShowWindow() {
	if h, _, _ := getConsoleWindow.Call(); h != 0 {
		const swShow = 5
		showWindow.Call(h, swShow)
	}
}

// Interactive reports whether stdin is an actual console — i.e. a human
// can answer prompts — as opposed to a pipe, file, or nothing.
func Interactive() bool {
	h, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	r, _, _ := getConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// LaunchedFromExplorer reports whether this process owns a brand-new
// console (double-click / "Open" in Explorer) rather than running inside
// an existing cmd/PowerShell session. When Explorer starts a console app,
// the app is the only process attached to its console.
func LaunchedFromExplorer() bool {
	var pids [4]uint32
	n, _, _ := getConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	return n == 1
}
