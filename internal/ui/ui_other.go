//go:build !windows

package ui

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

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

// openIniFile opens path in the user's default text editor. macOS's
// "open -t" always uses the default *text* editor (TextEdit unless the
// user changed it), regardless of what's registered for .ini files.
func openIniFile(path string) error {
	return exec.Command("open", "-t", path).Start()
}

// openInFileManager reveals path in the Finder, with it selected.
func openInFileManager(path string) error {
	return exec.Command("open", "-R", path).Start()
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

// pickFolder shows a native folder-choose dialog and returns the chosen
// absolute path, or "" if the user cancelled. This is the fallback the
// panel uses when it isn't hosted inside Mullion.app's WKWebView (which
// instead calls NSOpenPanel directly through its own JS bridge) — a
// plain macOS browser tab has no way to hand JS a folder's real path.
// Only macOS is supported: there's no single native dialog Mullion can
// rely on being installed on Linux.
func pickFolder(title string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("folder picker is not supported on this platform")
	}
	if title == "" {
		title = "Choose a folder"
	}
	prompt := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(title)
	// "tell me to activate" brings the dialog to the front without
	// triggering the automation-permission prompt that
	// "tell application \"System Events\" to activate" would.
	cmd := exec.Command("osascript",
		"-e", "tell me to activate",
		"-e", fmt.Sprintf(`POSIX path of (choose folder with prompt "%s")`, prompt))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// -128 is the cancel code; the message text is localized.
		if strings.Contains(stderr.String(), "(-128)") || strings.Contains(stderr.String(), "User canceled") {
			return "", nil
		}
		return "", fmt.Errorf("choosing folder: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSuffix(strings.TrimSpace(string(out)), "/"), nil
}

// pickFile shows a native file-choose dialog and returns the chosen
// absolute path, or "" if the user cancelled. types are file extensions
// without the leading dot (e.g. "sql", "gz") — empty/nil shows every
// file. Same darwin-only fallback rationale as pickFolder.
func pickFile(title string, types []string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("file picker is not supported on this platform")
	}
	if title == "" {
		title = "Choose a file"
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	prompt := esc.Replace(title)
	ofType := ""
	if len(types) > 0 {
		quoted := make([]string, len(types))
		for i, t := range types {
			quoted[i] = `"` + esc.Replace(t) + `"`
		}
		ofType = " of type {" + strings.Join(quoted, ", ") + "}"
	}
	cmd := exec.Command("osascript",
		"-e", "tell me to activate",
		"-e", fmt.Sprintf(`POSIX path of (choose file with prompt "%s"%s)`, prompt, ofType))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "(-128)") || strings.Contains(stderr.String(), "User canceled") {
			return "", nil
		}
		return "", fmt.Errorf("choosing file: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
