//go:build windows

package tty

import (
	"os"
	"syscall"
)

// IsTerminal reports whether f is attached to a real console by asking
// for its console mode: only a console handle answers that call
// successfully.
func IsTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
