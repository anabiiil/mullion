package app

// OPcache per PHP version: the PHP page's OPcache card and
// `mullion php opcache`. The php.ini work is phpver's (opcache.go);
// this adds the live status from the version's running php-fpm/php-cgi
// and the restarts that make a change take effect.

import (
	"fmt"
	"slices"

	"pm/internal/fcgi"
	"pm/internal/phpver"
)

// OpcacheInfo is one version's OPcache card.
type OpcacheInfo struct {
	Version  string         `json:"version"`
	Settings phpver.Opcache `json:"settings"`
	// Running: the version's php-fpm/php-cgi is up (it serves a site or
	// is the global version), so Status could be read from it.
	Running     bool                  `json:"running"`
	Status      *phpver.OpcacheStatus `json:"status,omitempty"`
	StatusError string                `json:"statusError,omitempty"`
	// JITNote explains a JIT that is configured but not running.
	JITNote string `json:"jitNote,omitempty"`
}

// OpcacheInfo reads a version's OPcache settings and, when its worker
// runs, the live status sites see.
func (a *App) OpcacheInfo(version string) (OpcacheInfo, error) {
	set, err := phpver.ReadOpcache(a.Paths, version)
	if err != nil {
		return OpcacheInfo{}, err
	}
	info := OpcacheInfo{Version: version, Settings: set}
	info.Running = slices.Contains(a.NeededVersions(), version) && len(fcgi.RunningVersions([]string{version})) == 1
	if !info.Running {
		return info, nil
	}
	st, err := phpver.LiveOpcacheStatus(a.Paths, version)
	if err != nil {
		info.StatusError = err.Error()
		return info, nil
	}
	info.Status = &st
	if set.JIT != "off" && st.Enabled && !st.JITEnabled {
		info.JITNote = "PHP turned the JIT off at startup — usually because an extension that hooks the executor is loaded " +
			"(on macOS the static builds include swoole and opentelemetry, which do), or the buffer is 0"
	}
	return info, nil
}

// SetOpcache applies a change to a version's php.ini and restarts its
// php-fpm/php-cgi so sites get it now.
func (a *App) SetOpcache(version string, c phpver.OpcacheChange) error {
	if c.Empty() {
		return fmt.Errorf("nothing to change")
	}
	if err := phpver.SetOpcache(a.Paths, version, c); err != nil {
		return err
	}
	return a.ResetOpcache(version)
}

// ResetOpcache empties a version's OPcache by restarting its
// php-fpm/php-cgi (a no-op when no site uses the version).
func (a *App) ResetOpcache(version string) error {
	if a.skipApply {
		return nil
	}
	return a.RestartPhp(version)
}
