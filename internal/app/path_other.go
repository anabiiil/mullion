//go:build !windows

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	pathBeginMarker = "# >>> managed by mullion — do not edit this block"
	pathEndMarker   = "# <<< managed by mullion"
)

// profileFiles returns the shell startup files the PATH block goes into:
// ~/.zprofile AND ~/.zshrc (zsh is the macOS default shell), plus the
// bash files when the user has them. The .zshrc copy matters: it loads
// after .zprofile, and our block sits at the END of it, so Mullion's
// prepend beats version managers (nvm-style PHP switchers) that prepend
// themselves earlier in the same file — without it, their entry shadows
// Mullion's php.
func profileFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	files := []string{
		filepath.Join(home, ".zprofile"),
		filepath.Join(home, ".zshrc"),
	}
	for _, f := range []string{".bash_profile", ".profile"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			files = append(files, filepath.Join(home, f))
		}
	}
	return files
}

// renderBlock builds the managed profile block: the given dirs prepended
// to PATH (so Mullion's php wins over Homebrew/system installs), and
// PHPRC pointing at the active version's directory so the `php` CLI
// picks up the php.ini Mullion writes there.
func renderBlock(dirs []string) string {
	home, _ := os.UserHomeDir()
	subst := func(p string) string {
		if home != "" && strings.HasPrefix(p, home+string(os.PathSeparator)) {
			return "$HOME" + strings.TrimPrefix(p, home)
		}
		return p
	}
	var b strings.Builder
	b.WriteString(pathBeginMarker + "\n")
	quoted := make([]string, len(dirs))
	phprc := ""
	for i, d := range dirs {
		quoted[i] = subst(d)
		if filepath.Base(d) == "current" {
			phprc = subst(d)
		}
	}
	b.WriteString(`export PATH="` + strings.Join(quoted, ":") + `:$PATH"` + "\n")
	if phprc != "" {
		b.WriteString(`export PHPRC="` + phprc + `"` + "\n")
	}
	b.WriteString(pathEndMarker + "\n")
	return b.String()
}

// upsertBlock replaces the managed block in one file (or appends it;
// empty block = remove). Reports whether the file changed.
func upsertBlock(path, block string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	lines := strings.Split(string(data), "\n")
	var kept []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == pathBeginMarker:
			inBlock = true
		case trimmed == pathEndMarker:
			inBlock = false
		case !inBlock:
			kept = append(kept, line)
		}
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	out := strings.Join(kept, "\n")
	if block != "" {
		if out != "" {
			out += "\n"
		}
		out += "\n" + block
	} else if out != "" {
		out += "\n"
	}
	if out == string(data) {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(out), 0o644)
}

// EnsureUserPath prepends dirs to PATH via a managed block in the
// shell profile files.
func EnsureUserPath(dirs ...string) error {
	block := renderBlock(dirs)
	changed := false
	for _, f := range profileFiles() {
		c, err := upsertBlock(f, block)
		if err != nil {
			return fmt.Errorf("updating %s: %w", f, err)
		}
		changed = changed || c
	}
	if changed {
		fmt.Println("Added to your PATH — open a NEW terminal for it to take effect.")
	}
	return nil
}

// EnsureMachinePathPriority is a no-op off Windows: the profile block
// already prepends Mullion's directories, so they win by construction.
func EnsureMachinePathPriority(dirs ...string) error { return nil }

// RemovePathEntries strips the managed block from the profile files
// (the scope argument only means something on Windows).
func RemovePathEntries(scope string, dirs ...string) error {
	for _, f := range profileFiles() {
		if _, err := upsertBlock(f, ""); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cleaning %s: %w", f, err)
		}
	}
	return nil
}

// sharedBinDirs are directories many tools live in — a PATH line adding
// one of these must never be touched (disabling the Homebrew line to
// beat a Homebrew php would break everything else brew installed).
var sharedBinDirs = map[string]bool{
	"/opt/homebrew/bin": true, "/opt/homebrew/sbin": true,
	"/usr/local/bin": true, "/usr/local/sbin": true,
	"/usr/bin": true, "/bin": true, "/usr/sbin": true, "/sbin": true,
}

// DisableShadowEntries comments out profile lines that put the given
// shadowing php's directory on the PATH — the fix for version managers
// (.pvm and friends) that keep beating Mullion. Lines adding shared
// directories are left alone (there the managed-block ordering wins
// instead). Returns a human-readable list of what was disabled.
func DisableShadowEntries(shadowExe string) ([]string, error) {
	dir := filepath.Dir(filepath.Clean(shadowExe))
	if sharedBinDirs[dir] {
		return nil, nil
	}
	variants := []string{dir}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home+string(os.PathSeparator)) {
		rest := strings.TrimPrefix(dir, home)
		variants = append(variants, "$HOME"+rest, "${HOME}"+rest, "~"+rest)
	}
	// Version managers wire themselves in via an init line, not a PATH
	// line — disabling THAT is what actually stops them.
	var initMarkers []string
	if strings.Contains(dir, string(os.PathSeparator)+".nvm"+string(os.PathSeparator)) {
		initMarkers = []string{"nvm.sh", "NVM_DIR"}
	}

	var disabled []string
	files := profileFiles()
	if home, err := os.UserHomeDir(); err == nil {
		if _, statErr := os.Stat(filepath.Join(home, ".bashrc")); statErr == nil {
			files = append(files, filepath.Join(home, ".bashrc"))
		}
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		changed := false
		inBlock := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			switch trimmed {
			case pathBeginMarker:
				inBlock = true
				continue
			case pathEndMarker:
				inBlock = false
				continue
			}
			if inBlock || trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			hit := false
			if strings.Contains(line, "PATH") {
				for _, v := range variants {
					if strings.Contains(line, v) {
						hit = true
						break
					}
				}
			}
			for _, m := range initMarkers {
				if strings.Contains(line, m) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			lines[i] = "# disabled by mullion (it shadowed Mullion's runtime — remove the marker to restore): " + line
			changed = true
			disabled = append(disabled, fmt.Sprintf("%s:%d", f, i+1))
		}
		if changed {
			if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				return disabled, fmt.Errorf("updating %s: %w", f, err)
			}
		}
	}
	return disabled, nil
}
