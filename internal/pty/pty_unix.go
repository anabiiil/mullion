//go:build darwin || linux

package pty

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// PTY is a running program attached to a pseudo-terminal. Read returns
// what the program prints; Write types into it.
type PTY struct {
	master *os.File
	cmd    *exec.Cmd

	closeOnce sync.Once
	waitOnce  sync.Once
	waitCode  int
	waitErr   error
	done      chan struct{}
}

// DefaultShell is the user's login shell ($SHELL, else zsh on macOS /
// bash on Linux), run as a login shell so their profile (and with it
// Mullion's PATH block) is loaded exactly like in Terminal.app.
func DefaultShell() (string, []string) {
	sh := os.Getenv("SHELL")
	if sh == "" || !filepath.IsAbs(sh) {
		sh = "/bin/zsh"
		if _, err := os.Stat(sh); err != nil {
			sh = "/bin/bash"
		}
	}
	if _, err := os.Stat(sh); err != nil {
		sh = "/bin/sh"
	}
	switch filepath.Base(sh) {
	case "zsh", "bash", "fish", "ksh", "tcsh", "csh", "sh", "dash":
		return sh, []string{"-l"}
	}
	return sh, nil
}

// Start runs cmd with args in dir under a new pseudo-terminal of the
// given size. env nil means os.Environ(); TERM and COLORTERM are always
// set so programs emit full colour.
func Start(cmd string, args []string, dir string, env []string, cols, rows uint16) (*PTY, error) {
	cols, rows = clampSize(cols, rows)
	master, slaveName, err := openMaster()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	defer slave.Close() // the child holds its own copies

	if err := setSize(master, cols, rows); err != nil {
		master.Close()
		return nil, err
	}

	if env == nil {
		env = os.Environ()
	}
	env = setEnv(env, "TERM", "xterm-256color")
	env = setEnv(env, "COLORTERM", "truecolor")

	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = slave, slave, slave
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := c.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return &PTY{master: master, cmd: c, done: make(chan struct{})}, nil
}

// Pid of the program running in the terminal.
func (p *PTY) Pid() int { return p.cmd.Process.Pid }

func (p *PTY) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	// Linux reports the slave side going away as EIO rather than EOF.
	if err != nil && errors.Is(err, syscall.EIO) {
		err = os.ErrClosed
	}
	return n, err
}

func (p *PTY) Write(b []byte) (int, error) { return p.master.Write(b) }

// Resize tells the terminal (and so the program, via SIGWINCH) its new size.
func (p *PTY) Resize(cols, rows uint16) error {
	cols, rows = clampSize(cols, rows)
	return setSize(p.master, cols, rows)
}

// Close hangs up the terminal: the shell's process group gets SIGHUP
// (like closing a Terminal window) and the master is closed, which
// unblocks any pending Read.
func (p *PTY) Close() error {
	var err error
	p.closeOnce.Do(func() {
		if p.cmd.Process != nil {
			// Negative pid = the whole session's process group.
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGHUP)
		}
		err = p.master.Close()
		go func() {
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			}
		}()
		go p.Wait()
	})
	return err
}

// Wait blocks until the program exits and returns its exit code.
func (p *PTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.waitCode = p.cmd.ProcessState.ExitCode()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = nil // a non-zero exit is reported through the code
		}
		p.waitErr = err
		close(p.done)
	})
	<-p.done
	return p.waitCode, p.waitErr
}

type winsize struct {
	Row, Col, X, Y uint16
}

func setSize(f *os.File, cols, rows uint16) error {
	ws := winsize{Row: rows, Col: cols}
	return ioctl(f, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

func ioctl(f *os.File, req, arg uintptr) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if cerr := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); cerr != nil {
		return cerr
	}
	if errno != 0 {
		return errno
	}
	return nil
}

func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+val)
}
