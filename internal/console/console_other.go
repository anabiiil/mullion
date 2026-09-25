//go:build !windows

package console

import (
	"os"

	"pm/internal/tty"
)

func LaunchedFromExplorer() bool { return false }

// Interactive reports whether a human can answer questions: stdin and
// stdout are both attached to a terminal.
func Interactive() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

func isTerminal(f *os.File) bool {
	return tty.IsTerminal(f)
}

func HideWindow() {}

func ShowWindow() {}

func FlushInput() {}
