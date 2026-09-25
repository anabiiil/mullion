//go:build linux

package pty

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// openMaster opens a new pseudo-terminal master on Linux and returns it
// with its slave device's path (unlockpt/ptsname via ioctls).
func openMaster() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var unlock int32
	if err := ioctl(m, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, "", err
	}
	var n uint32
	if err := ioctl(m, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, "", err
	}
	return m, "/dev/pts/" + strconv.Itoa(int(n)), nil
}

// Cwd returns the current working directory of process pid.
func Cwd(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
}
