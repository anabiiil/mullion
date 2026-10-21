package fcgi

// The live watcher on Windows (internal/watch): php-cgi has no per-site
// PHP_VALUE, so while any site has dumps or the deep watcher on, every
// php-cgi worker loads the watcher's ini dir (auto_prepend_file = the
// gate) through PHP_INI_SCAN_DIR. That is safe for unwatched sites: the
// gate returns at once unless Caddy passed that site's MULLION_WATCH
// FastCGI param. Only while needed — not always — so with nothing
// watched php-cgi loads nothing extra and a php.ini auto_prepend_file of
// the user's own stays in effect. php-fpm (macOS/Linux) honours
// PHP_VALUE, so there workers never change.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/watch"
)

// webWatchMarker exists while some site needs the prepend for web
// requests (written by SetWebWatch, from the Caddyfile generation).
func webWatchMarker(paths pmdir.Paths) string {
	return filepath.Join(paths.WatchDir(), "web-scan-dir")
}

// SetWebWatch records whether php-cgi workers should load the watcher.
func SetWebWatch(paths pmdir.Paths, on bool) error {
	m := webWatchMarker(paths)
	if !on {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if _, err := os.Stat(m); err == nil {
		return nil
	}
	if err := os.MkdirAll(paths.WatchDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(m, []byte("php-cgi workers load "+watch.IniDir(paths)+"\n"), 0o644)
}

// webWatchWanted: the marker is set and the ini it points at exists.
func webWatchWanted(paths pmdir.Paths) bool {
	if _, err := os.Stat(webWatchMarker(paths)); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(watch.IniDir(paths), "mullion-watch.ini"))
	return err == nil
}

// watchScanDir is PHP_INI_SCAN_DIR for a worker: the existing value with
// the watcher's dir appended — or, unset, a leading separator (an empty
// entry, which PHP reads as "the compiled-in scan dir") then ours. It
// never touches php.ini itself: that is found next to php-cgi as before.
func watchScanDir(existing, dir string) string {
	sep := string(os.PathListSeparator)
	if existing == "" {
		return sep + dir
	}
	for _, d := range filepath.SplitList(existing) {
		if strings.EqualFold(filepath.Clean(d), filepath.Clean(dir)) {
			return existing
		}
	}
	return existing + sep + dir
}

// watchEnv sets PHP_INI_SCAN_DIR in env for a worker that should load
// the watcher (the variable's name matched case-insensitively, Windows
// style).
func watchEnv(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	existing := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PHP_INI_SCAN_DIR") {
			existing = v
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PHP_INI_SCAN_DIR="+watchScanDir(existing, dir))
}

// watchStateFile records whether the running worker of a version was
// started with the watcher ("1") or without ("0").
func watchStateFile(paths pmdir.Paths, version string) string {
	sel, _ := phpver.ParseSelector(version)
	return filepath.Join(paths.PidsDir(), fmt.Sprintf("php-%d.%d.watch", sel.Major, sel.Minor))
}

func recordWatchState(paths pmdir.Paths, version string, on bool) {
	if !workerUsesScanDir {
		return
	}
	v := "0"
	if on {
		v = "1"
	}
	_ = os.WriteFile(watchStateFile(paths, version), []byte(v), 0o644)
}

// RestartForWatch stops the running workers started with a different
// watcher setting than now wanted, so Apply's Ensure brings them back
// with (or without) it. A no-op where workers don't use the scan dir.
func RestartForWatch(paths pmdir.Paths, versions []string) {
	if !workerUsesScanDir {
		return
	}
	want := "0"
	if webWatchWanted(paths) {
		want = "1"
	}
	for _, v := range RunningVersions(versions) {
		got, err := os.ReadFile(watchStateFile(paths, v))
		if err != nil && want == "0" {
			continue // started before the watcher existed: never had it
		}
		if strings.TrimSpace(string(got)) != want {
			_ = StopVersion(paths, v)
		}
	}
}
