// Package sysproc holds the platform-specific process and service
// plumbing shared by the CLI and the control panel: who owns a port,
// how to stop it FOR GOOD (including its launchd/brew manager), and how
// to enumerate or kill processes.
package sysproc

// Conflict describes a foreign process holding one of Mullion's ports.
type Conflict struct {
	Port int
	PID  int
	Name string
}
