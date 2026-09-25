//go:build !darwin && !linux && !windows

package pty

// PTY is unavailable on this platform; Start always fails.
type PTY struct{}

func DefaultShell() (string, []string) { return "/bin/sh", nil }

func Start(cmd string, args []string, dir string, env []string, cols, rows uint16) (*PTY, error) {
	return nil, ErrUnsupported
}

func Cwd(pid int) (string, error)             { return "", ErrUnsupported }
func (p *PTY) Pid() int                       { return 0 }
func (p *PTY) Read(b []byte) (int, error)     { return 0, ErrUnsupported }
func (p *PTY) Write(b []byte) (int, error)    { return 0, ErrUnsupported }
func (p *PTY) Resize(cols, rows uint16) error { return ErrUnsupported }
func (p *PTY) Close() error                   { return nil }
func (p *PTY) Wait() (int, error)             { return 0, ErrUnsupported }
