//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"syscall"

	"pm/internal/pmdir"
)

// maybeExecFresher hands control to the package manager's mullion when
// this process is a stale COPY inside ~/.mullion/bin. Old installs
// copied the binary there, and that copy leads the PATH — so `brew
// upgrade` never took effect. New installs symlink instead; this
// redirect self-heals machines that still carry a copy.
func maybeExecFresher() {
	if os.Getenv("MULLION_NO_REDIRECT") != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	paths, err := pmdir.New()
	if err != nil {
		return
	}
	dest := filepath.Join(paths.BinDir(), pmdir.ExeName("mullion"))
	self, err1 := os.Stat(exe)
	installed, err2 := os.Stat(dest)
	if err1 != nil || err2 != nil || !os.SameFile(self, installed) {
		return // not running the installed copy
	}
	if info, err := os.Lstat(dest); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return // already a symlink — always fresh
	}
	for _, brewBin := range []string{"/opt/homebrew/bin/mullion", "/usr/local/bin/mullion", "/home/linuxbrew/.linuxbrew/bin/mullion"} {
		if st, err := os.Stat(brewBin); err == nil && !os.SameFile(st, self) {
			env := append(os.Environ(), "MULLION_NO_REDIRECT=1")
			args := append([]string{brewBin}, os.Args[1:]...)
			_ = syscall.Exec(brewBin, args, env)
			return // Exec only returns on failure — carry on as ourselves
		}
	}
}
