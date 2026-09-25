//go:build windows

// Package shortcut manages the Mullion desktop shortcut. It points at
// the installed copy in ~/.mullion/bin — never at the file setup was
// launched from, which the user may delete afterwards.
package shortcut

import (
	"fmt"
	"pm/internal/proc"
	"path/filepath"
	"strings"

	"pm/internal/pmdir"
)

// CreateDesktop writes (or refreshes) Mullion.lnk on the user's Desktop.
// GetFolderPath resolves the real Desktop even when OneDrive redirects it.
func CreateDesktop(paths pmdir.Paths) error {
	exe := filepath.Join(paths.BinDir(), "mullion.exe")
	script := fmt.Sprintf(`
$desktop = [Environment]::GetFolderPath('Desktop')
$s = (New-Object -ComObject WScript.Shell).CreateShortcut((Join-Path $desktop 'Mullion.lnk'))
$s.TargetPath = %s
$s.WorkingDirectory = %s
$s.IconLocation = %s
$s.Description = 'Mullion - local dev server for Windows'
$s.Save()`,
		psQuote(exe), psQuote(paths.BinDir()), psQuote(exe+",0"))
	if out, err := proc.Quiet("powershell", "-NoProfile", "-Command", script).CombinedOutput(); err != nil {
		return fmt.Errorf("creating desktop shortcut: %v: %s", err, out)
	}
	return nil
}

// RemoveDesktop deletes the shortcut (no-op when absent).
func RemoveDesktop() error {
	script := `$lnk = Join-Path ([Environment]::GetFolderPath('Desktop')) 'Mullion.lnk'; if (Test-Path $lnk) { Remove-Item $lnk -Force }`
	if out, err := proc.Quiet("powershell", "-NoProfile", "-Command", script).CombinedOutput(); err != nil {
		return fmt.Errorf("removing desktop shortcut: %v: %s", err, out)
	}
	return nil
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
