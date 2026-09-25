package caddy

import (
	"os"
	"path/filepath"
	"runtime"
)

// RootCertPath returns the path to Caddy's locally-trusted root
// certificate, mirroring Caddy's own data-directory resolution
// (github.com/caddyserver/caddy AppDataDir): $XDG_DATA_HOME/caddy when
// set, else the OS default. Mullion never sets XDG_DATA_HOME for the
// Caddy process it starts (see internal/caddy/service.go), so on a
// stock machine this resolves to the platform default below.
func RootCertPath() string {
	return filepath.Join(dataDir(), "pki", "authorities", "local", "root.crt")
}

func dataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "caddy")
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("AppData"), "Caddy")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Caddy")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", "caddy")
	}
}
