package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"pm/internal/pmdir"
)

// PgAdminBundle is the pgAdmin 4 directory inside EDB's zips (under
// pgsql/): a signed app bundle on macOS, a plain folder on Windows.
func PgAdminBundle(goos string) string {
	if goos == "darwin" {
		return "pgAdmin 4.app"
	}
	return "pgAdmin 4"
}

// wantPgAdmin selects the pgAdmin 4 subtree of the zip — the part
// wantEntry leaves out for the server.
func wantPgAdmin(goos string) func(string) bool {
	prefix := "pgsql/" + PgAdminBundle(goos) + "/"
	return func(name string) bool { return strings.HasPrefix(name, prefix) }
}

// FetchPgAdmin downloads just the pgAdmin 4 that ships in the EDB zip of
// a PostgreSQL version (the same zip Install reads, with the opposite
// filter) and moves it to dest/<PgAdminBundle>, replacing any copy there.
func FetchPgAdmin(ctx context.Context, paths pmdir.Paths, version, dest string) error {
	if err := platformSupported(); err != nil {
		return err
	}
	if !versionRe.MatchString(version) {
		return fmt.Errorf("invalid PostgreSQL version %q", version)
	}
	bundle := PgAdminBundle(runtime.GOOS)
	staging := filepath.Join(paths.TmpDir(), "pgadmin-extract")
	defer os.RemoveAll(staging)
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return err
	}
	var lastErr error
	for _, url := range downloadURLs(version) {
		os.RemoveAll(staging)
		if lastErr = fetchZip(ctx, url, staging, paths.TmpDir(), wantPgAdmin(runtime.GOOS)); lastErr != nil {
			if strings.Contains(lastErr.Error(), "HTTP 4") {
				continue
			}
			return fmt.Errorf("downloading pgAdmin 4: %w", lastErr)
		}
		src := filepath.Join(staging, "pgsql", bundle)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("unexpected archive layout: no pgsql/%s in the %s zip", bundle, Label(version))
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		target := filepath.Join(dest, bundle)
		os.RemoveAll(target) // an older or half-finished copy
		return os.Rename(src, target)
	}
	if lastErr == nil {
		lastErr = errors.New("no download source for this platform")
	}
	return fmt.Errorf("downloading pgAdmin 4: %w", lastErr)
}
