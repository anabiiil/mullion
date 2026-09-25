package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"pm/internal/app"
	"pm/internal/autostart"
	"pm/internal/pmdir"
	"pm/internal/shortcut"
	"pm/internal/version"
)

// selfUpdateIfNeeded brings an existing install up to this executable's
// version: replaces bin\mullion.exe, refreshes the autostart script and
// the desktop shortcut, stamps the version file, and (re)starts the
// tray. Runs when a newer downloaded exe is double-clicked on a machine
// that was set up by an older release.
func selfUpdateIfNeeded(a *app.App) {
	installed := ""
	if data, err := os.ReadFile(a.Paths.VersionFile()); err == nil {
		installed = strings.TrimSpace(string(data))
	}
	if installed == version.Number {
		return
	}

	if installed == "" {
		fmt.Printf("Updating the Mullion install to v%s ...\n", version.Number)
	} else {
		fmt.Printf("New version! Updating Mullion v%s -> v%s ...\n", installed, version.Number)
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	dest := filepath.Join(a.Paths.BinDir(), pmdir.ExeName("mullion"))

	// Replace the installed copy unless we ARE the installed copy
	// (package-manager installs become symlinks so upgrades stick).
	if err := installSelf(a, exe); err != nil {
		fmt.Println("note: could not update the installed copy -", err)
		return
	}

	// The autostart script and shortcut may point at old behavior —
	// refresh them, but only if the user had them in the first place.
	if autostart.Enabled() {
		_ = autostart.Enable(a.Paths)
	}
	_ = shortcut.CreateDesktop(a.Paths)
	_ = os.WriteFile(a.Paths.VersionFile(), []byte(version.Number), 0o644)

	// Bring the tray up on the new version (no-op if already running).
	if runtime.GOOS == "windows" {
		spawnSelf(dest, "tray")
	}
	fmt.Printf("Mullion is now v%s.\n", version.Number)
}
