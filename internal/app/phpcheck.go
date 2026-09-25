package app

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// PhpShadow reports the foreign php executable that wins PATH resolution
// over Mullion's `current` junction, or "" when Mullion's php is the one found (or
// php is not on PATH at all). A typical culprit is another dev stack
// (Laragon, XAMPP, ...) sitting in the *system* PATH, which Windows
// always searches before the user PATH that Mullion installs itself into.
func (a *App) PhpShadow() string {
	found, err := exec.LookPath("php")
	if err != nil {
		return ""
	}
	dir := filepath.Clean(filepath.Dir(found))
	if strings.EqualFold(dir, filepath.Clean(a.Paths.CurrentPhp())) {
		return ""
	}
	return found
}
