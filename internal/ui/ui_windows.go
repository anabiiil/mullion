//go:build windows

package ui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pm/internal/pmdir"
	"pm/internal/proc"
)

// openTab opens url in the user's default browser.
func openTab(url string) {
	_ = proc.Quiet("cmd", "/c", "start", "", url).Start()
}

// chromiumBrowsers are the browsers that support --app windows.
var chromiumBrowsers = map[string]bool{
	"msedge.exe": true, "chrome.exe": true, "brave.exe": true,
	"vivaldi.exe": true, "opera.exe": true, "chromium.exe": true,
}

// defaultBrowser resolves the exe the user's https links open with.
func defaultBrowser() (string, bool) {
	out, _ := proc.Quiet("powershell", "-NoProfile", "-Command",
		`$p = (Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice' -ErrorAction SilentlyContinue).ProgId; if ($p) { (Get-ItemProperty ("Registry::HKEY_CLASSES_ROOT\" + $p + "\shell\open\command") -ErrorAction SilentlyContinue).'(default)' }`).Output()
	cmdline := strings.TrimSpace(string(out))
	if cmdline == "" {
		return "", false
	}
	exe := cmdline
	if strings.HasPrefix(cmdline, `"`) {
		if end := strings.Index(cmdline[1:], `"`); end > 0 {
			exe = cmdline[1 : 1+end]
		}
	} else if i := strings.Index(cmdline, ".exe"); i > 0 {
		exe = cmdline[:i+4]
	}
	if _, err := os.Stat(exe); err != nil {
		return "", false
	}
	return exe, true
}

// openAppWindow launches the panel as a standalone app-mode window — in
// the user's DEFAULT browser when it's Chromium-based, falling back to
// Edge/Chrome. The dedicated profile dir forces a separate process whose
// lifetime matches the window, so we know when it closes. Errors when no
// app-capable browser fits (e.g. the default is Firefox) — the caller
// then opens a plain tab in the default browser instead.
func openAppWindow(url string) (<-chan struct{}, error) {
	paths, err := pmdir.New()
	if err != nil {
		return nil, err
	}

	// A lingering browser from a previous panel session swallows the new
	// launch: Chromium single-instances per user-data-dir, so the fresh
	// process delegates to the old one and exits — which looks like
	// "nothing opened". Clear any old instance first.
	profile := strings.ReplaceAll(paths.Home+`\ui-profile`, "'", "''")
	_ = proc.Quiet("powershell", "-NoProfile", "-Command", fmt.Sprintf(
		`Get-CimInstance Win32_Process | Where-Object { ('msedge.exe','chrome.exe','brave.exe','vivaldi.exe','opera.exe','chromium.exe') -contains $_.Name -and $_.CommandLine -like '*%s*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		profile)).Run()

	var candidates []string
	if exe, ok := defaultBrowser(); ok {
		if !chromiumBrowsers[strings.ToLower(filepath.Base(exe))] {
			// The user's browser can't do app windows — respect their
			// choice with a normal tab rather than forcing Edge.
			return nil, errors.New("default browser has no app mode")
		}
		candidates = append(candidates, exe)
	}
	if v := os.Getenv("ProgramFiles(x86)"); v != "" {
		candidates = append(candidates, v+`\Microsoft\Edge\Application\msedge.exe`)
	}
	if v := os.Getenv("ProgramFiles"); v != "" {
		candidates = append(candidates,
			v+`\Microsoft\Edge\Application\msedge.exe`,
			v+`\Google\Chrome\Application\chrome.exe`)
	}
	if v := os.Getenv("LocalAppData"); v != "" {
		candidates = append(candidates, v+`\Google\Chrome\Application\chrome.exe`)
	}
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		// The sign-in/sync flags matter: on a fresh profile Edge silently
		// signs the Windows account in and shows a "syncing your data"
		// splash — the panel must be a plain window, nothing more.
		cmd := exec.Command(exe,
			"--app="+url,
			"--user-data-dir="+paths.Home+`\ui-profile`,
			"--window-size=1080,780",
			"--no-first-run", "--no-default-browser-check",
			"--disable-sync",
			"--disable-features=msImplicitSignin,msSeamlessWebToBrowserSignIn,msSyncPromoAfterImplicitSignIn,msFirstRunExperience,msEdgeWelcomePage,SyncPromo,SigninInterceptBubble")
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
