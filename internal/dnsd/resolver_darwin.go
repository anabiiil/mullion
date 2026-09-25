//go:build darwin

package dnsd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pm/internal/hosts"
)

// resolverDir is where macOS looks for per-domain resolver overrides.
const resolverDir = "/etc/resolver"

const resolverMarker = "# managed by mullion"

func resolverFile(tld string) string { return filepath.Join(resolverDir, tld) }

func resolverContent() string {
	return fmt.Sprintf("%s — wildcard DNS for local sites\nnameserver 127.0.0.1\nport %d\n", resolverMarker, Port)
}

// ResolverInstalled reports whether /etc/resolver/<tld> points at this
// server.
func ResolverInstalled(tld string) bool {
	data, err := os.ReadFile(resolverFile(tld))
	return err == nil && string(data) == resolverContent()
}

// InstallResolver writes /etc/resolver/<tld> (one administrator prompt)
// so macOS sends every *.<tld> lookup to this server. A resolver file
// for the TLD that Mullion didn't write (Valet's dnsmasq, say) is left
// alone and reported.
func InstallResolver(tld string) error {
	path := resolverFile(tld)
	if ResolverInstalled(tld) {
		return nil
	}
	if data, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(data), resolverMarker) {
		return fmt.Errorf("%s already exists and was not written by Mullion (another tool such as Valet/dnsmasq resolves .%s) — remove it first, or pick another TLD", path, tld)
	}
	tmp, err := os.CreateTemp("", "mullion-resolver-*.txt")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(resolverContent()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	script := fmt.Sprintf("/bin/mkdir -p %q && /bin/cp %q %q && /bin/chmod 644 %q && /usr/bin/dscacheutil -flushcache && /usr/bin/killall -HUP mDNSResponder",
		resolverDir, tmpPath, path, path)
	if err := hosts.RunElevated("to install "+path, script); err != nil {
		return fmt.Errorf("installing %s failed: %w", path, err)
	}
	if !ResolverInstalled(tld) {
		return fmt.Errorf("%s was not written; retry", path)
	}
	return nil
}

// RemoveResolver deletes /etc/resolver/<tld> if Mullion wrote it.
func RemoveResolver(tld string) error {
	path := resolverFile(tld)
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), resolverMarker) {
		return nil // absent, or someone else's — not ours to delete
	}
	script := fmt.Sprintf("/bin/rm -f %q && /usr/bin/dscacheutil -flushcache && /usr/bin/killall -HUP mDNSResponder", path)
	if err := hosts.RunElevated("to remove "+path, script); err != nil {
		return fmt.Errorf("removing %s failed: %w", path, err)
	}
	return nil
}

// ResolverDescription names the OS hookup, for status output.
func ResolverDescription(tld string) string { return resolverFile(tld) }
