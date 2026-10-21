//go:build windows

package diskfree

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// Free reports the free space (in bytes) available to an unprivileged
// user on the volume containing path. path need not exist yet — see
// existingAncestor.
func Free(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(existingAncestor(path))
	if err != nil {
		return 0, err
	}
	var freeBytesAvailable uint64
	ret, _, err := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		0,
		0,
	)
	if ret == 0 {
		return 0, err
	}
	return freeBytesAvailable, nil
}

// SameVolume reports whether a and b live on the same drive/volume,
// comparing filepath.VolumeName case-insensitively (Windows drive
// letters and UNC shares aren't case sensitive). Neither path needs to
// exist — see existingAncestor.
func SameVolume(a, b string) bool {
	va := filepath.VolumeName(existingAncestor(a))
	vb := filepath.VolumeName(existingAncestor(b))
	if va == "" || vb == "" {
		return false
	}
	return strings.EqualFold(va, vb)
}
