//go:build !windows

package vcredist

import (
	"context"

	"pm/internal/pmdir"
)

func Ensure(ctx context.Context, paths pmdir.Paths) error { return nil }
