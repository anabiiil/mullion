//go:build darwin

package shortcut

import (
	"pm/internal/macapp"
	"pm/internal/pmdir"
)

// CreateDesktop installs (or refreshes) Mullion.app in Applications —
// macOS has no desktop-shortcut equivalent of its own, so this is
// where the app bundle gets placed.
func CreateDesktop(paths pmdir.Paths) error {
	_, err := macapp.Install()
	return err
}

// RemoveDesktop removes any installed Mullion.app.
func RemoveDesktop() error {
	return macapp.Remove()
}
