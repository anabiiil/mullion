//go:build !windows && !darwin

package autostart

import (
	"fmt"

	"pm/internal/pmdir"
)

func Enable(paths pmdir.Paths) error {
	return fmt.Errorf("autostart is only supported on Windows and macOS")
}

func Disable() error { return nil }

func Enabled() bool { return false }
