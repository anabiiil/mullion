package phpver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"pm/internal/pmdir"
)

// setIniDirective sets "key=value" in the ini file at path: it replaces
// every existing active (uncommented) line for that key — allowing
// spaces around "=" — or appends one if the key isn't already set.
// Commented-out lines (";key=...") and every other line are left
// untouched. The file's mode is preserved, and it is created (0o644) if
// missing.
func setIniDirective(path, key, value string) error {
	mode := os.FileMode(0o644)
	var lines []string
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode()
		}
		lines = strings.Split(string(data), "\n")
	case os.IsNotExist(err):
		lines = nil
	default:
		return fmt.Errorf("reading %s: %w", path, err)
	}

	line := key + "=" + value
	found := false
	for i, l := range lines {
		if matchesIniKey(l, key) {
			lines[i] = line
			found = true
		}
	}
	if !found {
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines[n-1] = line
			lines = append(lines, "")
		} else {
			lines = append(lines, line)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), mode)
}

// matchesIniKey reports whether line is an active (not commented-out)
// "key=..." assignment for key, tolerating spaces around "=".
func matchesIniKey(line, key string) bool {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, ";") {
		return false
	}
	eq := strings.Index(l, "=")
	if eq < 0 {
		return false
	}
	return strings.TrimSpace(l[:eq]) == key
}

// IniSetting is one curated php.ini directive the control panel and CLI
// expose for editing, along with its effective (currently active) value.
type IniSetting struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Kind    string   `json:"kind"` // "size" | "int" | "bool" | "select" | "text"
	Options []string `json:"options,omitempty"`
	Value   string   `json:"value"` // effective value
}

// iniSettingDef is iniSettings' element type — IniSetting minus the
// effective Value, which only ListIni can fill in.
type iniSettingDef struct {
	Key     string
	Label   string
	Kind    string
	Options []string
}

// iniSettings is the curated, fixed-order list of php.ini directives the
// panel and CLI can view and edit. Order matters: it is also the order
// values come back in from the single php -r invocation in ListIni.
var iniSettings = []iniSettingDef{
	{Key: "memory_limit", Label: "Memory limit", Kind: "size"},
	{Key: "upload_max_filesize", Label: "Upload max filesize", Kind: "size"},
	{Key: "post_max_size", Label: "Post max size", Kind: "size"},
	{Key: "max_execution_time", Label: "Max execution time (seconds)", Kind: "int"},
	{Key: "max_input_time", Label: "Max input time (seconds)", Kind: "int"},
	{Key: "max_input_vars", Label: "Max input vars", Kind: "int"},
	{Key: "display_errors", Label: "Display errors", Kind: "bool"},
	{Key: "error_reporting", Label: "Error reporting", Kind: "select", Options: []string{
		"E_ALL", "E_ALL & ~E_DEPRECATED", "E_ALL & ~E_DEPRECATED & ~E_NOTICE",
	}},
	{Key: "date.timezone", Label: "Timezone", Kind: "text"},
	{Key: "opcache.validate_timestamps", Label: "Opcache validate timestamps", Kind: "bool"},
}

// findIniSetting looks up a curated setting by key.
func findIniSetting(key string) (iniSettingDef, bool) {
	for _, s := range iniSettings {
		if s.Key == key {
			return s, true
		}
	}
	return iniSettingDef{}, false
}

// ListIni resolves the effective value of every curated php.ini setting
// with ONE invocation of the version's php binary, rather than spawning
// one process per key. error_reporting's ini_get() comes back as a raw
// integer, so the same call also evaluates each of its Options as a PHP
// expression and ListIni maps the integer back to the option it matches
// (falling back to the raw number when nothing matches — e.g. a value
// set outside the curated options).
func ListIni(paths pmdir.Paths, version string) ([]IniSetting, error) {
	php := filepath.Join(paths.PhpVersionDir(version), PhpExeName)

	var code strings.Builder
	for _, s := range iniSettings {
		fmt.Fprintf(&code, `echo ini_get(%q), "\n";`, s.Key)
	}
	errReporting, _ := findIniSetting("error_reporting")
	for _, opt := range errReporting.Options {
		fmt.Fprintf(&code, `echo %s, "\n";`, opt)
	}

	out, err := exec.Command(php, "-r", code.String()).Output()
	if err != nil {
		return nil, fmt.Errorf("reading php.ini settings from PHP %s: %w", version, err)
	}
	lines := strings.Split(string(out), "\n")

	settings := make([]IniSetting, len(iniSettings))
	for i, s := range iniSettings {
		val := ""
		if i < len(lines) {
			val = strings.TrimSpace(lines[i])
		}
		if s.Kind == "bool" {
			val = iniBoolValue(val)
		}
		settings[i] = IniSetting{Key: s.Key, Label: s.Label, Kind: s.Kind, Options: s.Options, Value: val}
	}

	// Map error_reporting's raw integer back to the matching option.
	for i, s := range iniSettings {
		if s.Key != "error_reporting" {
			continue
		}
		optVals := lines[len(iniSettings):]
		for j, opt := range s.Options {
			if j < len(optVals) && strings.TrimSpace(optVals[j]) == settings[i].Value {
				settings[i].Value = opt
				break
			}
		}
	}

	// The CLI forces some settings (max_execution_time=0, even in
	// get_cfg_var) that php-fpm/php-cgi do honor for sites, so a value
	// php.ini sets explicitly wins over what the CLI reports.
	fileVals := iniFileValues(filepath.Join(paths.PhpVersionDir(version), "php.ini"))
	for i, s := range iniSettings {
		v, ok := fileVals[s.Key]
		if !ok {
			continue
		}
		switch s.Kind {
		case "bool":
			settings[i].Value = iniBoolValue(v)
		case "select":
			for _, opt := range s.Options {
				if v == opt {
					settings[i].Value = opt
				}
			}
		default:
			settings[i].Value = v
		}
	}

	return settings, nil
}

// iniFileValues returns the active key=value lines of an ini file (the
// last one wins, as in PHP), with surrounding quotes stripped.
func iniFileValues(path string) map[string]string {
	vals := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return vals
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		eq := strings.Index(l, "=")
		if l == "" || strings.HasPrefix(l, ";") || strings.HasPrefix(l, "[") || eq < 0 {
			continue
		}
		vals[strings.TrimSpace(l[:eq])] = strings.Trim(strings.TrimSpace(l[eq+1:]), `"'`)
	}
	return vals
}

// iniBoolValue normalizes an ini_get() result for a boolean-ish
// directive to "1" or "0", the way PHP treats such directives:
// "1"/"on"/"true"/"yes" (any case) is on, everything else is off.
func iniBoolValue(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "on", "true", "yes":
		return "1"
	default:
		return "0"
	}
}

var (
	iniSizeRe = regexp.MustCompile(`^-1$|^\d+[KMGkmg]?$`)
	iniIntRe  = regexp.MustCompile(`^-1$|^\d+$`)
)

// SetIni validates value against key's curated kind, writes it into the
// version's php.ini, and leaves restarting php-fpm/php-cgi to the
// caller. key must be one of the settings ListIni exposes.
func SetIni(paths pmdir.Paths, version, key, value string) error {
	setting, ok := findIniSetting(key)
	if !ok {
		return fmt.Errorf("%q is not a supported php.ini setting", key)
	}

	switch setting.Kind {
	case "size":
		if !iniSizeRe.MatchString(value) {
			return fmt.Errorf("invalid value %q for %s: expected -1 or a size like 128M", value, key)
		}
	case "int":
		if !iniIntRe.MatchString(value) {
			return fmt.Errorf("invalid value %q for %s: expected -1 or a whole number", value, key)
		}
	case "bool":
		switch strings.ToLower(value) {
		case "1", "on", "true":
			value = "1"
		case "0", "off", "false":
			value = "0"
		default:
			return fmt.Errorf("invalid value %q for %s: expected on or off", value, key)
		}
	case "select":
		found := false
		for _, opt := range setting.Options {
			if opt == value {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("invalid value %q for %s: must be one of %s", value, key, strings.Join(setting.Options, ", "))
		}
	case "text":
		if value == "" || len(value) > 100 || strings.ContainsAny(value, "\n\r\"';") {
			return fmt.Errorf("invalid value %q for %s: 1-100 characters, no quotes, semicolons, or newlines", value, key)
		}
		if key == "date.timezone" {
			php := filepath.Join(paths.PhpVersionDir(version), PhpExeName)
			cmd := exec.Command(php, "-r", `exit(in_array($argv[1], timezone_identifiers_list()) ? 0 : 1);`, value)
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("%q is not a valid timezone identifier", value)
			}
		}
	default:
		return fmt.Errorf("unsupported setting kind %q for %s", setting.Kind, key)
	}

	iniPath := filepath.Join(paths.PhpVersionDir(version), "php.ini")
	if err := setIniDirective(iniPath, key, value); err != nil {
		return fmt.Errorf("setting %s in %s: %w", key, iniPath, err)
	}
	return nil
}
