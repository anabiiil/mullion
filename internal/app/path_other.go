//go:build !windows

package app

import "fmt"

func EnsureUserPath(dirs ...string) error {
	fmt.Printf("(dev mode) add these to your PATH manually: %v\n", dirs)
	return nil
}

func EnsureMachinePathPriority(dirs ...string) error { return nil }

func RemovePathEntries(scope string, dirs ...string) error { return nil }
