//go:build !windows && !darwin

package postgres

import "fmt"

// edbPlatform: EDB publishes binary zips for Windows and macOS only.
const edbPlatform = ""

func platformSupported() error {
	return fmt.Errorf("PostgreSQL installs are not supported on this platform yet — use your distribution's packages")
}

func prepareBinaries(dir string) {}

const runHint = ""
