//go:build windows

// Package autostart makes `mullion start` run when the user signs in, via a
// small VBScript in the Startup folder (it launches the services with
// no console window flashing).
package autostart

import (
	"fmt"
	"os"
	"path/filepath"

	"pm/internal/pmdir"
)

func scriptPath() (string, error) {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return "", fmt.Errorf("APPDATA is not set")
	}
	return filepath.Join(appdata,
		"Microsoft", "Windows", "Start Menu", "Programs", "Startup", "mullion-autostart.vbs"), nil
}

// Enable registers `mullion start` to run at sign-in.
func Enable(paths pmdir.Paths) error {
	p, err := scriptPath()
	if err != nil {
		return err
	}
	exe := filepath.Join(paths.BinDir(), "mullion.exe")
	script := fmt.Sprintf("CreateObject(\"WScript.Shell\").Run \"\"\"%s\"\" start\", 0, False\r\n", exe)
	return os.WriteFile(p, []byte(script), 0o644)
}

// Disable removes the sign-in entry (no-op when not registered).
func Disable() error {
	p, err := scriptPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Enabled reports whether Mullion is registered to start at sign-in.
func Enabled() bool {
	p, err := scriptPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}
