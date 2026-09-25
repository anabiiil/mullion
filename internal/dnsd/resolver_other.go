//go:build !darwin && !windows

package dnsd

import "fmt"

// ResolverInstalled: no automatic hookup on this OS.
func ResolverInstalled(tld string) bool { return false }

// InstallResolver is not automated here (resolv.conf, systemd-resolved
// and NetworkManager all differ); point .<tld> at the server manually.
func InstallResolver(tld string) error {
	return fmt.Errorf("automatic wildcard DNS is only supported on macOS and Windows — point .%s at 127.0.0.1 port %d in your resolver (e.g. systemd-resolved) yourself", tld, Port)
}

// RemoveResolver is a no-op: nothing was installed.
func RemoveResolver(tld string) error { return nil }

// ResolverDescription names the OS hookup, for status output.
func ResolverDescription(tld string) string { return "manual resolver configuration" }
