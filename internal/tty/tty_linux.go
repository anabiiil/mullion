//go:build linux

package tty

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether f is attached to a real terminal by asking
// the kernel for its termios settings: only a terminal device answers
// that ioctl successfully.
func IsTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
