//go:build darwin || linux

package diskfree

import "syscall"

// Free reports the free space (in bytes) available to an unprivileged
// user on the volume containing path. path need not exist yet — see
// existingAncestor.
func Free(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(existingAncestor(path), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// SameVolume reports whether a and b live on the same filesystem
// volume (compared by device number). Neither path needs to exist —
// see existingAncestor.
func SameVolume(a, b string) bool {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(existingAncestor(a), &sa); err != nil {
		return false
	}
	if err := syscall.Stat(existingAncestor(b), &sb); err != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
