//go:build !darwin && !windows

package mterm

import (
	"context"
	"errors"
)

// Mullion itself supports only macOS and Windows; elsewhere Mullion
// Terminal is simply never found.

func Find() (string, bool) { return "", false }

func InstalledVersion(string) string { return "" }

func installFile(context.Context, string, string) error {
	return errors.New("installing Mullion Terminal isn't supported on this platform")
}

func launch(string, string) error {
	return errors.New("Mullion Terminal isn't supported on this platform")
}
