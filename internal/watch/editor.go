package watch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"pm/internal/proc"
)

// Editors the Watch tab can open a file:line in, in auto-detect order.
var Editors = []string{"vscode", "cursor", "phpstorm", "zed", "sublime"}

// editorCLI lists where each editor's launcher may be.
func editorCLI(name string) []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		switch name {
		case "vscode":
			return []string{"code", "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"}
		case "cursor":
			return []string{"cursor", "/Applications/Cursor.app/Contents/Resources/app/bin/cursor"}
		case "phpstorm":
			return []string{"phpstorm", "/Applications/PhpStorm.app/Contents/MacOS/phpstorm",
				filepath.Join(home, "Applications/PhpStorm.app/Contents/MacOS/phpstorm")}
		case "zed":
			return []string{"zed", "/Applications/Zed.app/Contents/MacOS/cli"}
		case "sublime":
			return []string{"subl", "/Applications/Sublime Text.app/Contents/SharedSupport/bin/subl"}
		}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		switch name {
		case "vscode":
			return []string{"code.cmd", "code", filepath.Join(local, `Programs\Microsoft VS Code\bin\code.cmd`)}
		case "cursor":
			return []string{"cursor.cmd", "cursor", filepath.Join(local, `Programs\cursor\resources\app\bin\cursor.cmd`)}
		case "phpstorm":
			return []string{"phpstorm64.exe", "phpstorm.cmd", "phpstorm"}
		case "zed":
			return []string{"zed"}
		case "sublime":
			return []string{"subl", `C:\Program Files\Sublime Text\subl.exe`}
		}
	default:
		switch name {
		case "vscode":
			return []string{"code"}
		case "sublime":
			return []string{"subl"}
		default:
			return []string{name}
		}
	}
	return nil
}

func findEditor(name string) string {
	for _, c := range editorCLI(name) {
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// AvailableEditors lists the editors found on this machine.
func AvailableEditors() []string {
	var out []string
	for _, e := range Editors {
		if findEditor(e) != "" {
			out = append(out, e)
		}
	}
	return out
}

// EditorArgs is the command line that opens file at line in an editor.
func EditorArgs(name, exe, file string, line int) []string {
	if line < 1 {
		line = 1
	}
	pos := file + ":" + strconv.Itoa(line)
	switch name {
	case "vscode", "cursor":
		return []string{exe, "-g", pos}
	case "phpstorm":
		return []string{exe, "--line", strconv.Itoa(line), file}
	default: // zed, sublime
		return []string{exe, pos}
	}
}

// OpenInEditor opens file:line — file must be inside root (the
// project), so the panel can't be used to open arbitrary paths. A
// relative file is taken from root. preferred "" auto-detects; with no
// editor found the OS opens the file with its default app.
func OpenInEditor(root, file string, line int, preferred string) (string, error) {
	if file == "" {
		return "", errors.New("no file to open")
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	file = filepath.Clean(file)
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the project", file)
	}
	if _, err := os.Stat(file); err != nil {
		return "", err
	}
	names := Editors
	if preferred != "" {
		names = append([]string{preferred}, Editors...)
	}
	for _, n := range names {
		if exe := findEditor(n); exe != "" {
			args := EditorArgs(n, exe, file, line)
			cmd := exec.Command(args[0], args[1:]...)
			proc.HideConsole(cmd) // code.cmd & co. would flash a console
			if err := cmd.Start(); err == nil {
				go cmd.Wait()
				return n, nil
			}
		}
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-t", file)
	case "windows":
		cmd = exec.Command("notepad.exe", file)
	default:
		cmd = exec.Command("xdg-open", file)
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go cmd.Wait()
	return "default", nil
}
