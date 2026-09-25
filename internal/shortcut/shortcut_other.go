//go:build !windows && !darwin

package shortcut

import "pm/internal/pmdir"

func CreateDesktop(paths pmdir.Paths) error { return nil }

func RemoveDesktop() error { return nil }
