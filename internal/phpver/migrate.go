package phpver

// Carrying one installed version's configuration over to a newer patch
// of the same branch (`mullion php update`): the php.ini is copied,
// with every line that points INTO the old version's directory
// re-pointed at the new one. Which lines that is depends on how the
// version was set up:
//
//   - extension="<verDir>/ext/ldap.so" and extension="<verDir>/ext/pdo_dblib.so"
//     (macOS: modules mullion built from php.net sources for that exact
//     patch — the new install builds its own, they are never copied);
//   - curl.cainfo / openssl.cafile = "<verDir>\extras\ssl\cacert.pem"
//     (Windows: the CA bundle inside the zip);
//   - extension_dir, when someone made it absolute (mullion writes the
//     relative "ext");
//   - anything else the user pointed at the version dir.
//
// Everything else — enabled/disabled extensions, OPcache, limits,
// Mailpit's sendmail_path, the watcher's lines — is version-independent
// text and comes along as is.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"pm/internal/pmdir"
)

// migrateGOOS is the platform whose extension rules apply — a seam for
// tests (Windows bare extension= names, DLL carry-over).
var migrateGOOS = runtime.GOOS

// MigrateReport says what MigrateIni changed.
type MigrateReport struct {
	// Repointed counts php.ini lines that referenced the old version's
	// directory and now reference the new one.
	Repointed int `json:"repointed"`
	// Disabled lists extensions the old php.ini loaded that the new
	// version doesn't have (their lines are commented out, with a note).
	Disabled []string `json:"disabled,omitempty"`
	// Added lists extensions the new install set up that the old php.ini
	// never mentioned (e.g. pdo_dblib built now but not back then).
	Added []string `json:"added,omitempty"`
	// Copied lists files carried over: Windows PECL DLLs (and the
	// runtime DLLs they ship) the new zip lacks, and conf.d files.
	Copied []string `json:"copied,omitempty"`
}

// MigrateIni carries version from's configuration over to version to,
// which must be freshly installed (its own generated php.ini is
// replaced). Windows extension DLLs the new build lacks are copied —
// builds of one branch share the extension API — but macOS shared
// modules (.so) never are: they are built per patch by the install.
func MigrateIni(paths pmdir.Paths, from, to string) (MigrateReport, error) {
	var rep MigrateReport
	oldDir, newDir := paths.PhpVersionDir(from), paths.PhpVersionDir(to)
	oldIni, newIni := filepath.Join(oldDir, "php.ini"), filepath.Join(newDir, "php.ini")

	oldData, err := os.ReadFile(oldIni)
	if os.IsNotExist(err) {
		return rep, nil // nothing to carry: keep the new version's own php.ini
	}
	if err != nil {
		return rep, fmt.Errorf("reading %s: %w", oldIni, err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(oldIni); err == nil {
		mode = info.Mode().Perm()
	}
	freshData, _ := os.ReadFile(newIni)

	copied, err := carryFiles(oldDir, newDir, migrateGOOS)
	rep.Copied = copied
	if err != nil {
		return rep, err
	}

	text, n := repointIni(string(oldData), oldDir, newDir, from, to)
	rep.Repointed = n
	lines := strings.Split(text, "\n")
	lines, rep.Disabled = disableMissingModules(lines, newDir, to, migrateGOOS)
	lines, rep.Added = addNewModules(lines, strings.Split(string(freshData), "\n"), to)

	if err := os.WriteFile(newIni, []byte(strings.Join(lines, "\n")), mode); err != nil {
		return rep, fmt.Errorf("writing %s: %w", newIni, err)
	}
	return rep, nil
}

// repointIni rewrites every reference to oldDir (as written, with
// forward slashes, or with doubled backslashes) into newDir, and the
// "built against PHP <from>" notes mullion writes, returning the new
// text and how many lines changed. A reference only counts when the
// directory name ends there — 8.4.2 must not match inside 8.4.23.
func repointIni(text, oldDir, newDir, from, to string) (string, int) {
	type pair struct{ old, new string }
	pairs := []pair{{oldDir, newDir}}
	if s := filepath.ToSlash(oldDir); s != oldDir {
		pairs = append(pairs, pair{s, filepath.ToSlash(newDir)})
	}
	if strings.Contains(oldDir, `\`) {
		pairs = append(pairs, pair{strings.ReplaceAll(oldDir, `\`, `\\`), strings.ReplaceAll(newDir, `\`, `\\`)})
	}
	lines := strings.Split(text, "\n")
	changed := 0
	for i, l := range lines {
		orig := l
		for _, p := range pairs {
			l = replaceDirRef(l, p.old, p.new)
		}
		l = strings.ReplaceAll(l, "built against PHP "+from+" ", "built against PHP "+to+" ")
		if l != orig {
			lines[i] = l
			if !strings.HasPrefix(strings.TrimSpace(orig), ";") {
				changed++
			}
		}
	}
	return strings.Join(lines, "\n"), changed
}

// replaceDirRef replaces old with new in s wherever old is a whole path
// prefix: followed by a separator, a quote, a ;-comment, whitespace or
// the end of the line.
func replaceDirRef(s, old, new string) string {
	if old == "" {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(old)
		if end == len(s) || strings.ContainsRune(`/\"'; `+"\t\r", rune(s[end])) {
			b.WriteString(s[:i])
			b.WriteString(new)
		} else {
			b.WriteString(s[:end])
		}
		s = s[end:]
	}
}

// extLineValue returns the unquoted value of an extension= line.
func extLineValue(line string) string {
	l := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ";"))
	eq := strings.Index(l, "=")
	if eq < 0 {
		return ""
	}
	v := strings.TrimSpace(l[eq+1:])
	if i := strings.Index(v, ";"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(v, `"'`)
}

// isAbsAnyOS treats both /unix and C:\windows paths as absolute, so the
// rules hold whichever platform the tests run on.
func isAbsAnyOS(p string) bool {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// disableMissingModules comments out active extension lines the new
// version can't load: absolute paths into newDir whose file is missing
// (macOS: a module whose rebuild failed), and on Windows bare names
// without an ext\php_<name>.dll. Each gets a note saying how to add it.
func disableMissingModules(lines []string, newDir, to, goos string) ([]string, []string) {
	var out, disabled []string
	for _, line := range lines {
		name, active, _ := parseExtLine(line)
		if name == "" || !active {
			out = append(out, line)
			continue
		}
		val := extLineValue(line)
		missing := false
		switch {
		case isAbsAnyOS(val):
			norm := filepath.ToSlash(val)
			if strings.HasPrefix(norm, filepath.ToSlash(newDir)+"/") && !fileExists(filepath.FromSlash(norm)) {
				missing = true
			}
		case goos == "windows" && !strings.ContainsAny(val, `/\`):
			ext := filepath.Join(newDir, "ext")
			if !fileExists(filepath.Join(ext, "php_"+name+".dll")) && !fileExists(filepath.Join(ext, val)) {
				missing = true
			}
		}
		if !missing {
			out = append(out, line)
			continue
		}
		disabled = append(disabled, name)
		// The note must not contain "=": parseExtLine would read it.
		out = append(out, fmt.Sprintf("; mullion: %s is not available for PHP %s yet — add it with: mullion php ext get %s %s", name, to, name, to),
			";"+strings.TrimRight(line, "\r"))
	}
	return out, disabled
}

// addNewModules appends the extension lines of the new version's own
// generated php.ini for modules the carried-over one never mentions,
// active or commented out — a module the user switched off stays off.
func addNewModules(lines, fresh []string, to string) ([]string, []string) {
	mentioned := map[string]bool{}
	for _, l := range lines {
		if name, _, _ := parseExtLine(l); name != "" {
			mentioned[name] = true
		}
	}
	var added []string
	for _, l := range fresh {
		name, active, _ := parseExtLine(l)
		if name == "" || !active || mentioned[name] {
			continue
		}
		if len(added) == 0 {
			for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
				lines = lines[:len(lines)-1]
			}
			lines = append(lines, "", "; Added by mullion when updating to PHP "+to)
		}
		lines = append(lines, strings.TrimRight(l, "\r"))
		mentioned[name] = true
		added = append(added, name)
	}
	if len(added) > 0 {
		lines = append(lines, "")
	}
	return lines, added
}

// carryFiles copies what the new version's directory lacks from the old
// one: conf.d/*.ini everywhere, plus on Windows ext\*.dll (PECL
// extensions such as redis or xdebug — one branch shares the extension
// API) and the top-level runtime DLLs they ship (imagick's
// CORE_RL_*.dll). Existing files are never overwritten. macOS .so
// modules are deliberately left alone (rebuilt per patch).
func carryFiles(oldDir, newDir, goos string) ([]string, error) {
	var copied []string
	carry := func(sub, pattern string) error {
		matches, _ := filepath.Glob(filepath.Join(oldDir, sub, pattern))
		for _, src := range matches {
			info, err := os.Stat(src)
			if err != nil || info.IsDir() {
				continue
			}
			dst := filepath.Join(newDir, sub, filepath.Base(src))
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := copyFile(src, dst, info.Mode().Perm()); err != nil {
				return fmt.Errorf("carrying %s over: %w", src, err)
			}
			copied = append(copied, filepath.Join(sub, filepath.Base(src)))
		}
		return nil
	}
	if err := carry("conf.d", "*.ini"); err != nil {
		return copied, err
	}
	if goos == "windows" {
		if err := carry("ext", "*.dll"); err != nil {
			return copied, err
		}
		if err := carry("", "*.dll"); err != nil {
			return copied, err
		}
	}
	return copied, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

// VerifyInstall runs the version's php with its php.ini and fails when
// it doesn't start cleanly — an extension that won't load prints a
// warning on every invocation, which is exactly what an update must not
// leave behind.
func VerifyInstall(paths pmdir.Paths, version string) error {
	php := filepath.Join(paths.PhpVersionDir(version), PhpExeName)
	out, err := exec.Command(php, iniArgs(php, "-r", "echo PHP_VERSION;")...).CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("PHP %s does not start: %v: %s", version, err, msg)
	}
	for _, bad := range []string{"Warning", "Unable to load", "Fatal error", "Startup:"} {
		if bytes.Contains(out, []byte(bad)) {
			return fmt.Errorf("PHP %s starts with problems: %s", version, msg)
		}
	}
	if !strings.HasSuffix(msg, version) {
		return fmt.Errorf("PHP %s reports version %q", version, msg)
	}
	return nil
}
