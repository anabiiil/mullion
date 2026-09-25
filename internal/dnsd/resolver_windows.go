//go:build windows

package dnsd

import (
	"fmt"
	"strings"

	"pm/internal/hosts"
	"pm/internal/proc"
)

// nrptComment tags the rules Mullion owns so removal never touches a
// rule someone else created for the same namespace.
const nrptComment = "mullion"

func namespace(tld string) string { return "." + tld }

// ResolverInstalled reports whether an NRPT rule sends .<tld> lookups to
// 127.0.0.1 (reading NRPT needs no elevation).
func ResolverInstalled(tld string) bool {
	ps := fmt.Sprintf(`@(Get-DnsClientNrptRule | Where-Object { $_.Comment -eq %s -and $_.Namespace -contains %s -and $_.NameServers -contains '127.0.0.1' }).Count`,
		hosts.PSQuote(nrptComment), hosts.PSQuote(namespace(tld)))
	out, err := proc.Quiet("powershell", "-NoProfile", "-Command", ps).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "0" && strings.TrimSpace(string(out)) != ""
}

// InstallResolver adds the NRPT rule (one UAC prompt). NRPT can't carry
// a port, so the agent's DNS server must also hold 127.0.0.1:53.
func InstallResolver(tld string) error {
	if ResolverInstalled(tld) {
		return nil
	}
	fmt.Println("Enabling wildcard DNS needs administrator rights — please accept the UAC prompt.")
	cmd := fmt.Sprintf("Add-DnsClientNrptRule -Namespace %s -NameServers '127.0.0.1' -Comment %s; Clear-DnsClientCache",
		hosts.PSQuote(namespace(tld)), hosts.PSQuote(nrptComment))
	if err := hosts.RunElevated("to add a DNS rule for ."+tld, cmd); err != nil {
		return fmt.Errorf("adding the NRPT rule for .%s failed (UAC declined?): %w", tld, err)
	}
	if !ResolverInstalled(tld) {
		return fmt.Errorf("the NRPT rule for .%s was not created; retry from an administrator terminal", tld)
	}
	return nil
}

// RemoveResolver deletes Mullion's NRPT rule for .<tld>.
func RemoveResolver(tld string) error {
	if !ResolverInstalled(tld) {
		return nil
	}
	cmd := fmt.Sprintf("Get-DnsClientNrptRule | Where-Object { $_.Comment -eq %s -and $_.Namespace -contains %s } | ForEach-Object { Remove-DnsClientNrptRule -Name $_.Name -Force }; Clear-DnsClientCache",
		hosts.PSQuote(nrptComment), hosts.PSQuote(namespace(tld)))
	if err := hosts.RunElevated("to remove the DNS rule for ."+tld, cmd); err != nil {
		return fmt.Errorf("removing the NRPT rule for .%s failed (UAC declined?): %w", tld, err)
	}
	return nil
}

// ResolverDescription names the OS hookup, for status output.
func ResolverDescription(tld string) string { return "NRPT rule for " + namespace(tld) }
