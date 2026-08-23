//go:build !windows

package ui

import (
	"errors"
	"os"
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

// chromiumApps are macOS browsers that support --app windows, in
// preference order.
var chromiumApps = []string{
	"Google Chrome.app/Contents/MacOS/Google Chrome",
	"Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	"Brave Browser.app/Contents/MacOS/Brave Browser",
	"Vivaldi.app/Contents/MacOS/Vivaldi",
	"Chromium.app/Contents/MacOS/Chromium",
}

// openAppWindow launches the panel as a standalone app-mode window in
// the first Chromium-based browser found. The dedicated profile dir
// forces a separate process whose lifetime matches the window, so we
// know when it closes. Errors when none is installed — the caller then
// opens a plain tab in the default browser instead.
func openAppWindow(url string) (<-chan struct{}, error) {
	paths, err := pmdir.New()
	if err != nil {
		return nil, err
	}
	profile := filepath.Join(paths.Home, "ui-profile")

	// A lingering browser from a previous panel session swallows the new
	// launch: Chromium single-instances per user-data-dir, so the fresh
	// process delegates to the old one and exits — which looks like
	// "nothing opened". Clear any old instance first.
	_ = exec.Command("pkill", "-f", profile).Run()

	var candidates []string
	home, _ := os.UserHomeDir()
	for _, app := range chromiumApps {
		candidates = append(candidates, filepath.Join("/Applications", app))
		if home != "" {
			candidates = append(candidates, filepath.Join(home, "Applications", app))
		}
	}
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		cmd := exec.Command(exe,
			"--app="+url,
			"--user-data-dir="+profile,
			"--window-size=1080,780",
			"--no-first-run", "--no-default-browser-check",
			"--disable-sync")
		if err := cmd.Start(); err != nil {
			continue
		}
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		return done, nil
	}
	return nil, errors.New("no app-mode browser found")
}
