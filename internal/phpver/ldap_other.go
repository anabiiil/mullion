//go:build !darwin

package phpver

import (
	"context"
	"errors"

	"pm/internal/pmdir"
)

// ErrLdapUnsupported is returned by EnsureLdap on every platform except
// macOS. This function is only ever reached from code built for !windows
// (install_other.go, ext_other.go) — meaning in practice only Linux — so
// on Windows it exists but is unreachable dead code: those static builds
// ship php_ldap.dll already compiled in, and install_windows.go /
// ext_windows.go never call it.
var ErrLdapUnsupported = errors.New("ldap cannot be added automatically on this platform: mullion only knows how to build it (from php.net sources against the macOS LDAP.framework) on macOS")

// EnsureLdap reports ErrLdapUnsupported everywhere but macOS (see
// ldap_darwin.go for the real implementation). Callers that only want a
// best-effort attempt during a PHP install — where LDAP is a bonus, not
// a requirement — should skip calling this outside darwin rather than
// surface the error as a failure; callers acting on an explicit user
// request to add ldap (`mullion php ext get ldap`) should propagate it.
func EnsureLdap(ctx context.Context, paths pmdir.Paths, version string) error {
	return ErrLdapUnsupported
}
