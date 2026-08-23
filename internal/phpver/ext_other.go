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

// staticBuildNote explains why extensions cannot be toggled off Windows.
const staticBuildNote = "this platform uses static PHP builds from static-php.dev with all extensions compiled in — there is nothing to enable or disable"

// ListExtensions asks the version's php binary what it has compiled in.
// Static builds have every extension baked in and always enabled.
func ListExtensions(paths pmdir.Paths, version string) ([]Ext, error) {
	php := filepath.Join(paths.PhpVersionDir(version), PhpExeName)
	out, err := exec.Command(php, "-m").Output()
	if err != nil {
		return nil, fmt.Errorf("running php -m for PHP %s: %w", version, err)
	}
	var exts []Ext
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
		exts = append(exts, Ext{Name: strings.ToLower(l), Enabled: true, Zend: zend})
	}
	return exts, nil
}

// SetExtension is not applicable to static builds.
func SetExtension(paths pmdir.Paths, version, name string, enable bool) error {
	return fmt.Errorf("cannot toggle %q: %s", name, staticBuildNote)
}

// InstallPeclExtension is not applicable to static builds. Most PECL
// favorites (redis, imagick, apcu, swoole, ...) are already compiled in.
func InstallPeclExtension(ctx context.Context, paths pmdir.Paths, version, name string) error {
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
