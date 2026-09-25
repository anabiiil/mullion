// Package pty runs a program attached to a pseudo-terminal, so it
// behaves exactly as it would in a real terminal window (colours, line
// editing, full-screen apps). It backs the control panel's built-in
// terminal and uses only the standard library: /dev/ptmx + ioctls on
// macOS and Linux, ConPTY (CreatePseudoConsole) on Windows.
package pty

import "errors"

// ErrUnsupported is returned by Start on platforms without a backend.
var ErrUnsupported = errors.New("pty: not supported on this platform")

// clampSize keeps a requested size sane: a 0x0 terminal makes shells
// and full-screen programs misbehave, and absurd sizes are never real.
func clampSize(cols, rows uint16) (uint16, uint16) {
	if cols < 2 {
		cols = 80
	}
	if rows < 1 {
		rows = 24
	}
	if cols > 1000 {
		cols = 1000
	}
	if rows > 500 {
		rows = 500
	}
	return cols, rows
}
