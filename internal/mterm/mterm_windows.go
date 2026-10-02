//go:build windows

package mterm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/proc"
)

const exeName = AppName + ".exe"

// searchDirs lists where the NSIS installer puts the app: per-user
// (%LOCALAPPDATA%\Programs, its default) or per-machine (Program
// Files). Overridable for tests.
var searchDirs = func() []string {
	var dirs []string
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		dirs = append(dirs, filepath.Join(d, "Programs", AppName))
	}
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)"} {
		if d := os.Getenv(env); d != "" {
			dirs = append(dirs, filepath.Join(d, AppName))
		}
	}
	return dirs
}

// Find locates an installed "Mullion Terminal.exe".
func Find() (string, bool) {
	for _, d := range searchDirs() {
		p := filepath.Join(d, exeName)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return "", false
}

// InstalledVersion reads the exe's product version, which
// electron-builder stamps with the app version ("" if unreadable).
func InstalledVersion(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		"(Get-Item -LiteralPath $env:MT_EXE).VersionInfo.ProductVersion")
	cmd.Env = append(os.Environ(), "MT_EXE="+exe)
	proc.HideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	// "0.1.5.0" → "0.1.5"
	if parts := strings.Split(v, "."); len(parts) == 4 && parts[3] == "0" {
		v = strings.Join(parts[:3], ".")
	}
	return v
}

// installFile runs the downloaded NSIS installer silently and waits for
// it to finish. /currentuser picks the per-user install (no UAC prompt)
// that the installer's own page would otherwise ask about.
func installFile(ctx context.Context, installer, _ string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, installer, "/S", "/currentuser")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the Mullion Terminal installer failed: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// launch starts the exe with dir as its argument: the app is
// single-instance, so a running copy opens dir as a new tab. No
// proc.Detach — its HideWindow would start the window invisible; a GUI
// process isn't tied to our console anyway.
func launch(exe, dir string) error {
	var cmd *exec.Cmd
	if dir != "" {
		cmd = exec.Command(exe, dir)
		cmd.Dir = dir
	} else {
		cmd = exec.Command(exe)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening Mullion Terminal: %w", err)
	}
	return cmd.Process.Release()
}
