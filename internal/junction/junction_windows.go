//go:build windows

// Package junction manages the ~/.mullion/php/current directory link that puts
// the active PHP version on PATH. Junctions are used (not symlinks) because
// creating them requires no elevation on Windows.
package junction

import (
	"fmt"
	"os"
	"os/exec"
)

// Set points linkPath at targetDir, replacing any existing link.
func Set(linkPath, targetDir string) error {
	if _, err := os.Lstat(linkPath); err == nil {
		// os.Remove maps to RemoveDirectory, which deletes the junction
		// itself without touching the target's contents.
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("removing old link %s: %w", linkPath, err)
		}
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", linkPath, targetDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mklink /J failed: %v: %s", err, out)
	}
	return nil
}

// Target resolves where the link currently points ("" if absent).
func Target(linkPath string) string {
	t, err := os.Readlink(linkPath)
	if err != nil {
		return ""
	}
	return t
}
