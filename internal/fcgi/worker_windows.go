//go:build windows

package fcgi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"pm/internal/phpver"
	"pm/internal/pmdir"
)

// workerCmd builds the php-cgi command that serves one version's port.
func workerCmd(paths pmdir.Paths, version string, port int) (*exec.Cmd, error) {
	exe := filepath.Join(paths.PhpVersionDir(version), phpver.PhpCgiName)
	if _, err := os.Stat(exe); err != nil {
		return nil, fmt.Errorf("php-cgi not found for %s: %w", version, err)
	}
	cmd := exec.Command(exe, "-b", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(),
		// Never recycle the worker: keeps the single process alive forever.
		"PHP_FCGI_MAX_REQUESTS=0",
	)
	return cmd, nil
}

func killWorker(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
