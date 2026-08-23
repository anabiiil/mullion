//go:build !windows

package console

import "os"

func LaunchedFromExplorer() bool { return false }

// Interactive reports whether a human can answer questions: stdin and
// stdout are both attached to a terminal.
func Interactive() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func HideWindow() {}

func ShowWindow() {}

func FlushInput() {}
