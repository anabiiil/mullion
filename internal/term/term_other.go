//go:build !windows

package term

import (
	"os"

	"pm/internal/tty"
)

func initPlatform() bool {
	return tty.IsTerminal(os.Stdout)
}
