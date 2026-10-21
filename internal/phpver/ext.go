package phpver

import (
	"path/filepath"
	"strings"
)

// Ext is one extension of an installed PHP version.
type Ext struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Zend       bool   `json:"zend"`
	Toggleable bool   `json:"toggleable"`
}

func normalizeExtName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "php_")
	name = strings.TrimSuffix(name, ".so")
	return strings.TrimSuffix(name, ".dll")
}

// iniArgs pins a php invocation to the php.ini next to the binary.
// Without it a PHPRC in the environment (mullion's shell integration
// exports one for the global version) makes php load another version's
// ini — and its extension= lines — when inspecting this one.
func iniArgs(php string, args ...string) []string {
	return append([]string{"-c", filepath.Join(filepath.Dir(php), "php.ini")}, args...)
}

// parseExtLine recognizes `extension=` / `zend_extension=` lines,
// commented or not, and normalizes the extension name.
func parseExtLine(line string) (name string, active, zend bool) {
	l := strings.TrimSpace(line)
	active = true
	if strings.HasPrefix(l, ";") {
		active = false
		l = strings.TrimSpace(strings.TrimPrefix(l, ";"))
	}
	eq := strings.Index(l, "=")
	if eq < 0 {
		return "", false, false
	}
	key := strings.ToLower(strings.TrimSpace(l[:eq]))
	if key != "extension" && key != "zend_extension" {
		return "", false, false
	}
	val := strings.TrimSpace(l[eq+1:])
	if i := strings.Index(val, ";"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	val = strings.Trim(val, `"'`)
	val = filepath.Base(strings.ReplaceAll(val, `\`, "/"))
	val = normalizeExtName(val)
	if val == "" {
		return "", false, false
	}
	return val, active, key == "zend_extension"
}
