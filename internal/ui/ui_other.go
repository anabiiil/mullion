//go:build !windows

package ui

import (
	"errors"
	"os/exec"
	"path/filepath"

	"pm/internal/pmdir"
)

// openTab opens url in the user's default browser.
func openTab(url string) {
	opener := "open" // macOS
	if _, err := exec.LookPath(opener); err != nil {
		opener = "xdg-open"
	}
	_ = exec.Command(opener, url).Start()
}

// openAppWindow: on macOS the panel deliberately does NOT get its own
// app-mode Chromium window — that window carries Chrome's dock icon,
// spawns phantom windows from dock clicks, and hijacks link clicks into
// a browser the user may not use. Returning an error routes the caller
// to the fallback: a normal tab in the user's DEFAULT browser, where
// the icon, dock behavior, and links are all what they expect.
func openAppWindow(url string) (<-chan struct{}, error) {
	// Clean up any app-window instance left over from older versions.
	if paths, err := pmdir.New(); err == nil {
		_ = exec.Command("pkill", "-f", filepath.Join(paths.Home, "ui-profile")).Run()
	}
	return nil, errors.New("app windows are not used on this platform")
}
