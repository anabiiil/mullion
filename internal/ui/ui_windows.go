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

// openIniFile opens path in Notepad. Quiet just hides the console
// Notepad itself doesn't spawn, keeping this consistent with every
// other short-lived helper Mullion shells out to.
func openIniFile(path string) error {
	return proc.Quiet("notepad.exe", path).Start()
}

// openInFileManager reveals path in Explorer, with it selected.
func openInFileManager(path string) error {
	return proc.Quiet("explorer.exe", "/select,"+path).Start()
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

// pickFolder shows a native folder-choose dialog and returns the chosen
// absolute path, or "" if the user cancelled. FolderBrowserDialog needs
// an STA thread, hence -STA; the hidden TopMost owner form is what
// makes the dialog come to the front of the Edge app window instead of
// popping up behind it.
func pickFolder(title string) (string, error) {
	if title == "" {
		title = "Choose a folder"
	}
	script := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form
$owner.StartPosition = 'CenterScreen'
$owner.Size = New-Object System.Drawing.Size(0, 0)
$owner.ShowInTaskbar = $false
$owner.TopMost = $true
$owner.Show() | Out-Null
$owner.Activate() | Out-Null
$dlg = New-Object System.Windows.Forms.FolderBrowserDialog
$dlg.Description = %s
$dlg.ShowNewFolderButton = $true
if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) {
	Write-Output $dlg.SelectedPath
}
$owner.Close()`, psQuote(title))
	out, err := proc.Quiet("powershell", "-NoProfile", "-STA", "-Command", script).Output()
	if err != nil {
		return "", fmt.Errorf("choosing folder: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// pickFile shows a native file-choose dialog and returns the chosen
// absolute path, or "" if the user cancelled. types are file extensions
// without the leading dot (e.g. "sql", "gz") — empty/nil allows any file.
func pickFile(title string, types []string) (string, error) {
	if title == "" {
		title = "Choose a file"
	}
	filter := "All files (*.*)|*.*"
	if len(types) > 0 {
		exts := make([]string, len(types))
		for i, t := range types {
			exts[i] = "*." + t
		}
		filter = strings.ToUpper(strings.Join(types, "/")) + " files (" + strings.Join(exts, "; ") + ")|" + strings.Join(exts, ";")
	}
	script := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form
$owner.StartPosition = 'CenterScreen'
$owner.Size = New-Object System.Drawing.Size(0, 0)
$owner.ShowInTaskbar = $false
$owner.TopMost = $true
$owner.Show() | Out-Null
$owner.Activate() | Out-Null
$dlg = New-Object System.Windows.Forms.OpenFileDialog
$dlg.Title = %s
$dlg.Filter = %s
$dlg.Multiselect = $false
if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) {
	Write-Output $dlg.FileName
}
$owner.Close()`, psQuote(title), psQuote(filter))
	out, err := proc.Quiet("powershell", "-NoProfile", "-STA", "-Command", script).Output()
	if err != nil {
		return "", fmt.Errorf("choosing file: %v", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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
