//go:build windows

package term

import (
	"syscall"
	"unsafe"
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode = kernel32.NewProc("GetConsoleMode")
	setConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const enableVirtualTerminalProcessing = 0x0004

func initPlatform() bool {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return false
	}
	var mode uint32
	if r, _, _ := getConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false // not a console (piped/redirected)
	}
	r, _, _ := setConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
