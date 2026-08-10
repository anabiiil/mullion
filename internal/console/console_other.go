//go:build !windows

package console

func LaunchedFromExplorer() bool { return false }

func Interactive() bool { return false }

func HideWindow() {}

func ShowWindow() {}

func FlushInput() {}
