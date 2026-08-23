// Package composer installs the Composer dependency manager into
// ~/.mullion/bin as a phar plus a shim that runs it with Mullion's active PHP.
package composer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"pm/internal/download"
	"pm/internal/pmdir"
)

func PharPath(paths pmdir.Paths) string {
	return filepath.Join(paths.BinDir(), "composer.phar")
}

// Ensure installs the latest Composer only when none is present yet
// (what `mullion setup` wants: never touch an existing install).
func Ensure(ctx context.Context, paths pmdir.Paths) error {
	if _, err := os.Stat(PharPath(paths)); err == nil {
		return writeShim(paths)
	}
	return Install(ctx, paths, "")
}

// Install downloads the given Composer version ("" = latest stable)
// over whatever is currently installed.
func Install(ctx context.Context, paths pmdir.Paths, version string) error {
	url := "https://getcomposer.org/download/latest-stable/composer.phar"
	if version != "" {
		url = fmt.Sprintf("https://getcomposer.org/download/%s/composer.phar", version)
	}
	fmt.Println("Downloading Composer...")
	if err := download.ToFile(ctx, url, PharPath(paths)); err != nil {
		return err
	}
	return writeShim(paths)
}

// writeShim (re)creates the `composer` entry point. It reaches php
// through the `current` junction directly (not through PATH), so it
// works even in terminals opened before setup.
func writeShim(paths pmdir.Paths) error {
	if runtime.GOOS == "windows" {
		shim := "@echo off\r\n\"%~dp0..\\php\\current\\php.exe\" \"%~dp0composer.phar\" %*\r\n"
		return os.WriteFile(filepath.Join(paths.BinDir(), "composer.bat"), []byte(shim), 0o755)
	}
	shim := `#!/bin/sh
dir="$(cd "$(dirname "$0")" && pwd)"
exec "$dir/../php/current/php" -c "$dir/../php/current/php.ini" "$dir/composer.phar" "$@"
`
	return os.WriteFile(filepath.Join(paths.BinDir(), "composer"), []byte(shim), 0o755)
}
