//go:build !darwin

package phpver

import (
	"context"
	"errors"

	"pm/internal/pmdir"
)

// ErrSpxUnsupported is returned by EnsureSpx everywhere but macOS (see
// spx.go for why: SPX has no Windows build, and Linux has no mullion
// PHP builds to add it to).
var ErrSpxUnsupported = errors.New("the profiler is only available on macOS for now: SPX, the profiler mullion uses, has no Windows build")

// SpxSupported reports whether EnsureSpx can build SPX here.
func SpxSupported() bool { return false }

// EnsureSpx reports ErrSpxUnsupported (see spx_darwin.go).
func EnsureSpx(ctx context.Context, paths pmdir.Paths, version string) error {
	return ErrSpxUnsupported
}

// DisableSpx reports ErrSpxUnsupported (see spx_darwin.go).
func DisableSpx(paths pmdir.Paths, version string) error {
	return ErrSpxUnsupported
}
