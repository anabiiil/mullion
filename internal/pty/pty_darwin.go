//go:build darwin

package pty

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// openMaster opens a new pseudo-terminal master on macOS and returns it
// with its slave device's path (grantpt/unlockpt/ptsname via ioctls).
func openMaster() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ioctl(m, syscall.TIOCPTYGRANT, 0); err != nil {
		m.Close()
		return nil, "", err
	}
	if err := ioctl(m, syscall.TIOCPTYUNLK, 0); err != nil {
		m.Close()
		return nil, "", err
	}
	var name [128]byte
	if err := ioctl(m, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		m.Close()
		return nil, "", err
	}
	if i := bytes.IndexByte(name[:], 0); i >= 0 {
		return m, string(name[:i]), nil
	}
	return m, string(name[:]), nil
}

// Cwd returns the current working directory of process pid, read with
// the proc_info syscall (what lsof uses) — no process spawning.
func Cwd(pid int) (string, error) {
	const (
		procInfoCallPidinfo  = 2
		procPidVnodePathInfo = 9
		vnodeInfoSize        = 152 // struct vnode_info
		maxPath              = 1024
	)
	// struct proc_vnodepathinfo { vnode_info_path cdir, rdir }.
	var buf [2 * (vnodeInfoSize + maxPath)]byte
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallPidinfo, uintptr(pid),
		procPidVnodePathInfo, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return "", errno
	}
	if int(n) < vnodeInfoSize+maxPath {
		return "", syscall.EINVAL
	}
	path := buf[vnodeInfoSize : vnodeInfoSize+maxPath]
	if i := bytes.IndexByte(path, 0); i >= 0 {
		path = path[:i]
	}
	if len(path) == 0 {
		return "", syscall.ENOENT
	}
	return string(path), nil
}
