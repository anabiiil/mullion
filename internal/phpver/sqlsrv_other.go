//go:build !darwin && !windows

package phpver

import (
	"context"
	"errors"

	"pm/internal/pmdir"
)

// ErrSQLServerUnsupported is returned by EnsureSQLServer on platforms
// other than macOS and Windows: mullion only knows how to add SQL Server
// support to its macOS static builds (pdo_dblib + FreeTDS) and to the
// windows.php.net builds (Microsoft's sqlsrv/pdo_sqlsrv).
var ErrSQLServerUnsupported = errors.New("SQL Server support cannot be added automatically on this platform (only macOS and Windows)")

func sqlServerRuntimeReady() bool { return false }

// EnsureSQLServer reports ErrSQLServerUnsupported (see sqlsrv_darwin.go
// and sqlsrv_windows.go for the real implementations).
func EnsureSQLServer(ctx context.Context, paths pmdir.Paths, version string) error {
	return ErrSQLServerUnsupported
}
