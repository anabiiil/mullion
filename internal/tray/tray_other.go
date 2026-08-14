//go:build !windows

package tray

import "fmt"

type Handlers struct {
	OpenDashboard func()
	StartServices func()
	StopServices  func()
	OpenPMA       func()
}

var ErrAlreadyRunning = fmt.Errorf("the Mullion tray is already running")

func Run(h Handlers) error {
	return fmt.Errorf("the tray icon is only supported on Windows")
}
