//go:build !darwin

package macapp

// Install is a no-op outside macOS — there is no Mullion.app to place.
func Install() (string, error) { return "", nil }

// Remove is a no-op outside macOS.
func Remove() error { return nil }
