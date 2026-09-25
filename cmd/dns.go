package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/dnsd"
	"pm/internal/term"
)

var dnsCmd = &cobra.Command{
	Use:   "dns [on|off|status]",
	Short: "Wildcard DNS for *.<tld> — needed for '*' subdomain aliases",
	Long: `The hosts file can't express wildcards, so tenant1.shop.test only resolves
through Mullion's own tiny DNS server (run by the background agent).

  mullion dns on       install the OS hookup (asks for administrator rights once)
  mullion dns off      remove it
  mullion dns status   show what's in place (default)

macOS: /etc/resolver/<tld> sends *.<tld> lookups to 127.0.0.1 port 53535.
Windows: an NRPT rule sends .<tld> lookups to 127.0.0.1 (port 53, which must
be free — Internet Connection Sharing or a local DNS proxy can hold it).`,
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"on", "off", "status"},
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		action := "status"
		if len(args) == 1 {
			action = args[0]
		}
		tld := a.State.Config.TLD
		switch action {
		case "on":
			if err := a.SetWildcardDNS(true); err != nil {
				return err
			}
			fmt.Printf("Wildcard DNS is on: every *.%s name resolves to this machine.\n", tld)
		case "off":
			if err := a.SetWildcardDNS(false); err != nil {
				return err
			}
			fmt.Println("Wildcard DNS is off (named subdomains still resolve through the hosts file).")
			return nil
		case "status":
		default:
			return fmt.Errorf("unknown action %q (use on, off or status)", action)
		}
		enabled, resolver, server, note := a.WildcardDNSStatus()
		yn := func(ok bool) string {
			if ok {
				return term.Green("yes")
			}
			return term.Red("no")
		}
		fmt.Printf("wildcard dns: %s\n", map[bool]string{true: "on", false: "off"}[enabled])
		fmt.Printf("resolver (%s): %s\n", dnsd.ResolverDescription(tld), yn(resolver))
		fmt.Printf("dns server (%s): %s\n", dnsd.ProbeAddr(), yn(server))
		if note != "" {
			fmt.Println("note:", note)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(dnsCmd)
}
