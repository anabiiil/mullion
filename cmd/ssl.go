package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/term"
)

var sslCmd = &cobra.Command{
	Use:   "ssl",
	Short: "Show the local certificate authority behind https://*.test",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		info := a.SSLInfo()
		fmt.Println("Local CA:  ", info.CAPath)
		if !info.CAExists {
			fmt.Println("            not created yet — Caddy creates it on the first HTTPS request (`mullion secure`).")
		} else {
			fmt.Println("Subject:   ", info.CASubject)
			fmt.Println("Expires:   ", info.CAExpires)
			if info.Trusted {
				fmt.Println("Trusted:   ", term.Green("yes"))
			} else {
				fmt.Println("Trusted:   ", term.Red("no")+" — run `mullion ssl trust`")
			}
		}
		fmt.Printf("HTTPS sites: %d of %d\n", info.SecureSites, info.TotalSites)
		fmt.Println(term.Dim("Firefox and phones keep their own trust store: `mullion ssl export <file>` and import it there."))
		return nil
	},
}

var sslTrustCmd = &cobra.Command{
	Use:   "trust",
	Short: "Add the local CA to the system trust store (asks for your password / UAC)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.TrustCA(); err != nil {
			return err
		}
		fmt.Println(term.Green("✓ The local CA is trusted — restart the browser if it still warns."))
		return nil
	},
}

var sslExportCmd = &cobra.Command{
	Use:   "export <file>",
	Short: "Copy the local CA certificate (for Firefox, phones, VMs)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.ExportCA(args[0]); err != nil {
			return err
		}
		fmt.Println("Exported the local CA to", app.ExportCATarget(args[0]))
		return nil
	},
}

var sslAllCmd = &cobra.Command{
	Use:       "all <on|off>",
	Short:     "Serve every site over HTTPS (on) or plain HTTP (off)",
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"on", "off"},
	RunE: func(cmd *cobra.Command, args []string) error {
		var secure bool
		switch args[0] {
		case "on":
			secure = true
		case "off":
		default:
			return fmt.Errorf("use `mullion ssl all on` or `mullion ssl all off`")
		}
		a := mustApp()
		if err := a.SecureAll(secure); err != nil {
			return err
		}
		if !secure {
			fmt.Println("Every site is served over plain HTTP now.")
			return nil
		}
		if !a.SSLInfo().Trusted {
			if err := a.TrustCA(); err != nil {
				fmt.Println("note:", err)
				fmt.Println("If the browser warns about the certificate, run `mullion ssl trust`.")
			}
		}
		fmt.Println(term.Green("✓ Every site is served over HTTPS now."))
		return nil
	},
}

func init() {
	sslCmd.AddCommand(sslTrustCmd, sslExportCmd, sslAllCmd)
	rootCmd.AddCommand(sslCmd)
}
