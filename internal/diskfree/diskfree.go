// Package diskfree reports how much free space is available on the
// volume that holds a given path, without pulling in any third-party
// module — just the syscalls the OS already offers.
package diskfree

import (
	"os"
	"path/filepath"
)

// existingAncestor walks up from path until it finds a directory (or
// file) that actually exists, so Free and SameVolume can be asked
// about a path that hasn't been created yet — e.g. Mullion's MySQL
// data directory before EnsureInitialized has run for the first time.
// Stat-based syscalls (and Windows' GetDiskFreeSpaceExW) need a real
// path; a nonexistent one's nearest existing ancestor always lives on
// the same volume, so the answer is unaffected.
func existingAncestor(path string) string {
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
