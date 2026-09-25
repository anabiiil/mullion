//go:build darwin

// Package autostart brings the stack up at sign-in. On macOS that is a
// per-user launchd agent that runs `mullion start` when you log in.
package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"pm/internal/pmdir"
)

const agentLabel = "dev.mullion.start"

func plistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist")
}

func Enable(paths pmdir.Paths) error {
	plist := plistPath()
	if plist == "" {
		return fmt.Errorf("could not resolve your home directory")
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	exe := filepath.Join(paths.BinDir(), "mullion")
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>start</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, agentLabel, exe,
		filepath.Join(paths.LogsDir(), "autostart.log"),
		filepath.Join(paths.LogsDir(), "autostart.log"))
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	// Register with launchd right away; already-loaded is not an error.
	_ = exec.Command("launchctl", "unload", plist).Run()
	if out, err := exec.Command("launchctl", "load", "-w", plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %v: %s", err, out)
	}
	return nil
}

func Disable() error {
	plist := plistPath()
	if plist == "" {
		return nil
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return nil
	}
	_ = exec.Command("launchctl", "unload", "-w", plist).Run()
	return os.Remove(plist)
}

func Enabled() bool {
	plist := plistPath()
	if plist == "" {
		return false
	}
	_, err := os.Stat(plist)
	return err == nil
}
