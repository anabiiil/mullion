package phpver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pm/internal/pmdir"
)

// Ext is one loadable extension of an installed PHP version.
type Ext struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Zend    bool   `json:"zend"`
}

// ListExtensions returns every extension DLL the version's build ships,
// with whether its php.ini currently loads it.
func ListExtensions(paths pmdir.Paths, version string) ([]Ext, error) {
	dir := paths.PhpVersionDir(version)
	entries, err := os.ReadDir(filepath.Join(dir, "ext"))
	if err != nil {
		return nil, fmt.Errorf("PHP %s has no ext directory: %w", version, err)
	}
	enabled, err := enabledExtensions(filepath.Join(dir, "php.ini"))
	if err != nil {
		return nil, err
	}

	var out []Ext
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if !strings.HasPrefix(name, "php_") || !strings.HasSuffix(name, ".dll") {
			continue
		}
		name = strings.TrimSuffix(strings.TrimPrefix(name, "php_"), ".dll")
		out = append(out, Ext{Name: name, Enabled: enabled[name], Zend: name == "opcache"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetExtension enables or disables one extension in the version's
// php.ini. Enabling uncomments an existing line when there is one (the
// stock php.ini ships them commented) and appends one otherwise;
// disabling comments out every active line.
func SetExtension(paths pmdir.Paths, version, name string, enable bool) error {
	name = normalizeExtName(name)
	dir := paths.PhpVersionDir(version)
	if _, err := os.Stat(filepath.Join(dir, "ext", "php_"+name+".dll")); err != nil {
		return fmt.Errorf("PHP %s does not ship extension %q (no ext\\php_%s.dll)", version, name, name)
	}
	iniPath := filepath.Join(dir, "php.ini")
	data, err := os.ReadFile(iniPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", iniPath, err)
	}

	key := "extension"
	if name == "opcache" {
		key = "zend_extension"
	}

	lines := strings.Split(string(data), "\n")
	changed, satisfied := false, false
	for i, line := range lines {
		n, active, _ := parseExtLine(line)
		if n != name {
			continue
		}
		if enable {
			if active {
				satisfied = true
			} else if !satisfied {
				lines[i] = key + "=" + name
				satisfied, changed = true, true
			}
		} else if active {
			lines[i] = ";" + strings.TrimRight(line, "\r")
			changed = true
		}
	}
	if enable && !satisfied {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, key+"="+name, "")
		changed = true
	}

	if !changed {
		return nil
	}
	return os.WriteFile(iniPath, []byte(strings.Join(lines, "\n")), 0o644)
}

func enabledExtensions(iniPath string) (map[string]bool, error) {
	data, err := os.ReadFile(iniPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", iniPath, err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if name, active, _ := parseExtLine(line); active && name != "" {
			set[name] = true
		}
	}
	return set, nil
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

func normalizeExtName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "php_")
	return strings.TrimSuffix(name, ".dll")
}
