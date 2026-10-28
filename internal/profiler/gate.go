package profiler

// The request gate: for "the next N requests" and "requests matching a
// path", SPX is switched on for the site with automatic start off
// (spx.http_profiling_enabled=1, spx.http_profiling_auto_start=0) and
// this small file, loaded as auto_prepend_file, calls
// spx_profiler_start() for the requests it picks. Its counter lives in
// the site's gate.json, updated under flock, so concurrent php-fpm
// workers never profile more than N.
//
// php-fpm keeps a PHP_ADMIN_VALUE for later requests its worker serves,
// so the gate may run for another site: it does nothing unless Caddy
// passed this site's MULLION_SPX param (per request, never kept). The
// site's own prepend (the live watcher's, or php.ini's) still runs: Caddy
// passes it as MULLION_SPX_CHAIN.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"pm/internal/pmdir"
)

// gateSource is the PHP gate. It must stay valid on PHP 7.0+ and never
// throw: it runs before the site's code on every gated request.
const gateSource = `<?php
// Mullion profiler gate — written by mullion, recreated if changed.
// Loaded (auto_prepend_file) for sites profiling "the next N requests" or
// "requests matching a path"; does nothing for any other request.
(function () {
    $cfg = isset($_SERVER['MULLION_SPX']) ? (string) $_SERVER['MULLION_SPX'] : '';
    if ($cfg === '' || !function_exists('spx_profiler_start')) {
        return;
    }
    // SPX's own web UI, and a browser session SPX's control panel
    // switched on, are handled by SPX itself.
    if (isset($_GET['SPX_UI_URI']) || isset($_COOKIE['SPX_ENABLED']) || isset($_GET['SPX_ENABLED'])) {
        return;
    }
    $uri = isset($_SERVER['REQUEST_URI']) ? (string) $_SERVER['REQUEST_URI'] : '/';
    $path = parse_url($uri, PHP_URL_PATH);
    if (!is_string($path) || $path === '') {
        $path = '/';
    }
    $fh = @fopen($cfg, 'c+');
    if (!$fh) {
        return;
    }
    $start = false;
    if (@flock($fh, LOCK_EX)) {
        $c = json_decode((string) stream_get_contents($fh), true);
        if (is_array($c)) {
            $left = isset($c['remaining']) ? (int) $c['remaining'] : 0;
            $want = isset($c['path']) ? (string) $c['path'] : '';
            $match = $want === ''
                || (strpbrk($want, '*?[') === false
                    ? strncmp($path, $want, strlen($want)) === 0
                    : fnmatch($want, $path));
            if ($match && $left !== 0) {
                $start = true;
                $c['captured'] = (isset($c['captured']) ? (int) $c['captured'] : 0) + 1;
                if ($left > 0) {
                    $c['remaining'] = $left - 1;
                }
                ftruncate($fh, 0);
                rewind($fh);
                fwrite($fh, json_encode($c));
                fflush($fh);
            }
        }
        @flock($fh, LOCK_UN);
    }
    fclose($fh);
    if ($start) {
        spx_profiler_start();
    }
})();
if (isset($_SERVER['MULLION_SPX_CHAIN']) && $_SERVER['MULLION_SPX_CHAIN'] !== '' && is_file($_SERVER['MULLION_SPX_CHAIN'])) {
    require $_SERVER['MULLION_SPX_CHAIN'];
}
`

// GatePath is the gate's PHP file.
func GatePath(paths pmdir.Paths) string { return filepath.Join(Dir(paths), "gate.php") }

// gateConfigPath is a site's gate state.
func gateConfigPath(paths pmdir.Paths, site string) string {
	return filepath.Join(SiteDir(paths, site), "gate.json")
}

// GateState is a site's gate.json: Remaining requests to profile (-1 =
// no limit), the Path filter, and how many were Captured so far.
type GateState struct {
	Remaining int    `json:"remaining"`
	Path      string `json:"path,omitempty"`
	Captured  int    `json:"captured"`
}

// EnsureGate (re)writes gate.php when it is missing or differs.
func EnsureGate(paths pmdir.Paths) error {
	p := GatePath(paths)
	if data, err := os.ReadFile(p); err == nil && string(data) == gateSource {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(gateSource), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// gateStateFor is the gate.json a site's settings start with.
func gateStateFor(s *Site) GateState {
	g := GateState{Remaining: -1, Path: s.Path}
	if s.Mode == ModeNext {
		g.Remaining = s.Count
	}
	return g
}

// writeGate resets a site's gate.json for its settings.
func writeGate(paths pmdir.Paths, site string, s *Site) error {
	if err := os.MkdirAll(SiteDir(paths, site), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(gateStateFor(s))
	if err != nil {
		return err
	}
	return os.WriteFile(gateConfigPath(paths, site), data, 0o644)
}

// ReadGate returns a site's gate state (ok false: none).
func ReadGate(paths pmdir.Paths, site string) (GateState, bool) {
	var g GateState
	data, err := os.ReadFile(gateConfigPath(paths, site))
	if err != nil {
		return g, false
	}
	if json.Unmarshal(data, &g) != nil {
		return g, false
	}
	return g, true
}

// Prepare writes what a profiled site needs on disk before Caddy points
// PHP at it: the reports dir (spx.data_dir), and for gated modes the gate
// and the site's gate.json — started afresh when reset (the settings
// changed), else only when missing (regenerating the Caddyfile must not
// restart a "next N" count).
func Prepare(paths pmdir.Paths, site string, s *Site, reset bool) error {
	if err := os.MkdirAll(ReportsDir(paths, site), 0o755); err != nil {
		return err
	}
	if !s.Mode.usesGate() {
		err := os.Remove(gateConfigPath(paths, site))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := EnsureGate(paths); err != nil {
		return err
	}
	if _, ok := ReadGate(paths, site); ok && !reset {
		return nil
	}
	return writeGate(paths, site, s)
}
