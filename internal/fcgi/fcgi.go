// Package fcgi keeps one php-cgi FastCGI process running per PHP version
// in use, each listening on its version-derived local port.
package fcgi

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

func pidFile(paths pmdir.Paths, version string) string {
	sel, _ := phpver.ParseSelector(version)
	return filepath.Join(paths.PidsDir(), fmt.Sprintf("php-%d.%d.pid", sel.Major, sel.Minor))
}

// Ensure starts php-cgi for the given full version if it isn't already
// serving its port.
func Ensure(paths pmdir.Paths, version string) error {
	port, err := phpver.FcgiPort(version)
	if err != nil {
		return err
	}
	if portListening(port) {
		return nil
	}

	logPath := filepath.Join(paths.LogsDir(), fmt.Sprintf("php-%s.log", version))
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd, err := workerCmd(paths, version, port)
	if err != nil {
		return err
	}
	cmd.Dir = paths.PhpVersionDir(version)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the PHP %s FastCGI worker: %w", version, err)
	}
	pid := cmd.Process.Pid
	// Detach: the process must outlive us.
	_ = cmd.Process.Release()

	if err := os.WriteFile(pidFile(paths, version), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}

	// Give it a moment and confirm the port came up.
	for i := 0; i < 20; i++ {
		if portListening(port) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the PHP %s FastCGI worker did not start listening on port %d (see %s)", version, port, logPath)
}

// StopVersion kills the php-cgi process serving one version's branch
// and waits for its port to be released, so a fresh worker can pick up
// php.ini changes.
func StopVersion(paths pmdir.Paths, version string) error {
	path := pidFile(paths, version)
	if data, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			killWorker(pid)
		}
		_ = os.Remove(path)
	}
	if port, err := phpver.FcgiPort(version); err == nil {
		for i := 0; i < 20 && portListening(port); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// StopAll kills every php-cgi we started and clears the pid files.
func StopAll(paths pmdir.Paths) error {
	entries, err := os.ReadDir(paths.PidsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "php-") || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		path := filepath.Join(paths.PidsDir(), e.Name())
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				killWorker(pid)
			}
		}
		if err := os.Remove(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RunningVersions lists the major.minor branches whose port is serving.
func RunningVersions(versions []string) []string {
	var out []string
	for _, v := range versions {
		if port, err := phpver.FcgiPort(v); err == nil && portListening(port) {
			out = append(out, v)
		}
	}
	return out
}

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
