//go:build !windows && !darwin

package mongodb

import "fmt"

// MongoDB's Linux builds are per-distribution (ubuntu2204, rhel93, ...);
// picking the right one isn't implemented yet.
const (
	indexTarget   = ""
	indexArch     = ""
	mongoshDistro = ""
	runHint       = ""
)

var serverEntries []string

func platformSupported() error {
	return fmt.Errorf("MongoDB installs are not supported on this platform yet")
}

func serverFallbackURL(version string) string { return "" }

func mongoshURL(version string) string { return "" }

func prepareBinaries(dir string) {}
