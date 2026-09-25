//go:build windows

// Package vcredist installs the Microsoft Visual C++ 2015-2022 x64
// runtime when it's missing. PHP's Windows builds and MySQL both need
// it, and clean Windows 10/11 installs don't ship it.
package vcredist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"pm/internal/download"
	"pm/internal/pmdir"
)

const installerURL = "https://aka.ms/vs/17/release/vc_redist.x64.exe"

// Ensure checks for the runtime DLLs and silently installs the
// redistributable when any is missing. Needs administrator rights for a
// silent install; without them the installer shows its own UAC prompt.
func Ensure(ctx context.Context, paths pmdir.Paths) error {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	sys32 := filepath.Join(root, "System32")
	missing := false
	for _, dll := range []string{"vcruntime140.dll", "vcruntime140_1.dll", "msvcp140.dll"} {
		if _, err := os.Stat(filepath.Join(sys32, dll)); err != nil {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}

	fmt.Println("Installing the Microsoft Visual C++ runtime (required by PHP and MySQL)...")
	exe := filepath.Join(paths.TmpDir(), "vc_redist.x64.exe")
	if err := download.ToFile(ctx, installerURL, exe); err != nil {
		return err
	}
	defer os.Remove(exe)

	err := exec.Command(exe, "/install", "/quiet", "/norestart").Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 3010: // success, reboot pending
			return nil
		case 1638: // a newer version is already installed
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("vc_redist.x64.exe install failed: %w", err)
	}
	return nil
}
