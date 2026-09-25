//go:build !windows

package phpver

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"pm/internal/pmdir"
)

// staticBuildNote explains why most extensions cannot be toggled off
// Windows: these PHP builds come from static-php.dev with every
// extension compiled in, so there is no DLL/module to load or unload.
// A couple of them (opcache, apcu) still have a real ini on/off switch
// even though their code is always resident — see iniToggleKey.
const staticBuildNote = "this platform uses static PHP builds from static-php.dev with all extensions compiled in — only extensions with an ini on/off switch (opcache, apcu) can be toggled here"

// iniToggleOrder lists, in a fixed order, the extensions whose runtime
// activity is controlled by an ini directive rather than by compiling
// the code in or out. Order is fixed so the single php -r invocation in
// readIniToggles and its output stay in lockstep.
var iniToggleOrder = []string{"opcache", "apcu"}

// iniToggleKey maps a canonical extension name to the ini directive
// that switches it on or off.
var iniToggleKey = map[string]string{
	"opcache": "opcache.enable",
	"apcu":    "apc.enabled",
}

// canonicalExtName normalizes a name the way normalizeExtName does, and
// additionally folds php -m's "[Zend Modules]" entry for OPcache
// ("zend opcache") to the ini/CLI name "opcache".
func canonicalExtName(name string) string {
	name = normalizeExtName(name)
	if name == "zend opcache" {
		return "opcache"
	}
	return name
}

// ListExtensions asks the version's php binary what it has compiled in.
// Everything is always present in these static builds; a couple of
// extensions (opcache, apcu) are also ini-toggleable, so their Enabled
// reflects the ini setting rather than always being true.
func ListExtensions(paths pmdir.Paths, version string) ([]Ext, error) {
	php := filepath.Join(paths.PhpVersionDir(version), PhpExeName)
	out, err := exec.Command(php, "-m").Output()
	if err != nil {
		return nil, fmt.Errorf("running php -m for PHP %s: %w", version, err)
	}
	toggles, err := readIniToggles(php)
	if err != nil {
		return nil, err
	}
	var exts []Ext
	seen := map[string]int{} // name -> index into exts, so a name listed
	// under both [PHP Modules] and [Zend Modules] (static-php-cli does
	// this for OPcache) becomes one entry, not two.
	zend := false
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "" || l == "[PHP Modules]":
			continue
		case l == "[Zend Modules]":
			zend = true
			continue
		}
		name := canonicalExtName(strings.ToLower(l))
		if i, ok := seen[name]; ok {
			if zend {
				exts[i].Zend = true
			}
			continue
		}
		e := Ext{Name: name, Enabled: true, Zend: zend}
		if _, ok := iniToggleKey[name]; ok {
			e.Enabled, e.Toggleable = toggles[name], true
		}
		exts = append(exts, e)
		seen[name] = len(exts) - 1
	}
	return exts, nil
}

// readIniToggles runs the php binary once to read every ini-toggleable
// directive's effective value (php.ini plus any defaults), rather than
// spawning one php process per extension.
func readIniToggles(php string) (map[string]bool, error) {
	var code strings.Builder
	for _, name := range iniToggleOrder {
		fmt.Fprintf(&code, `echo ini_get(%q), "\n";`, iniToggleKey[name])
	}
	out, err := exec.Command(php, "-r", code.String()).Output()
	if err != nil {
		return nil, fmt.Errorf("reading ini settings from php: %w", err)
	}
	lines := strings.Split(string(out), "\n")
	result := make(map[string]bool, len(iniToggleOrder))
	for i, name := range iniToggleOrder {
		val := ""
		if i < len(lines) {
			val = lines[i]
		}
		result[name] = iniBoolOn(val)
	}
	return result, nil
}

// iniBoolOn interprets an ini_get() result the way PHP treats a
// boolean-ish directive: "1"/"on"/"true" (any case) is on, everything
// else (including empty, "0", "off") is off.
func iniBoolOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "on", "true":
		return true
	default:
		return false
	}
}

// SetExtension toggles the ini directive for the handful of extensions
// that have one (opcache, apcu). Everything else on a static build has
// no on/off switch at all.
func SetExtension(paths pmdir.Paths, version, name string, enable bool) error {
	canon := canonicalExtName(name)
	key, ok := iniToggleKey[canon]
	if !ok {
		return fmt.Errorf("cannot toggle %q: %s", name, staticBuildNote)
	}
	value := "0"
	if enable {
		value = "1"
	}
	iniPath := filepath.Join(paths.PhpVersionDir(version), "php.ini")
	if err := setIniDirective(iniPath, key, value); err != nil {
		return fmt.Errorf("setting %s in %s: %w", key, iniPath, err)
	}
	return nil
}

// InstallPeclExtension is not applicable to static builds for most PECL
// favorites (redis, imagick, apcu, swoole, ...) — they're already
// compiled in. ldap is the one exception: the static builds ship
// without it, so it's built from php.net sources instead (macOS only;
// see EnsureLdap in ldap_darwin.go / ldap_other.go).
func InstallPeclExtension(ctx context.Context, paths pmdir.Paths, version, name string) error {
	if normalizeExtName(name) == "ldap" {
		return EnsureLdap(ctx, paths, version)
	}
	exts, err := ListExtensions(paths, version)
	if err == nil {
		for _, e := range exts {
			if e.Name == normalizeExtName(name) {
				return fmt.Errorf("%s is already compiled into this PHP build", e.Name)
			}
		}
	}
	return fmt.Errorf("cannot add %q: %s", name, staticBuildNote)
}
