package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
)

var tldCmd = &cobra.Command{
	Use:   "tld [new-tld]",
	Short: "Show or change the domain suffix for all sites (default: test)",
	Long: `With no argument, prints the current TLD. With an argument, changes it —
e.g. ` + "`mullion tld local`" + ` makes every site resolve as <name>.local.

Avoid real public TLDs like .dev or .app: browsers force them to HTTPS
(HSTS preload), which breaks plain-HTTP local sites.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if len(args) == 0 {
			fmt.Println(a.State.Config.TLD)
			return nil
		}
		tld, err := app.ValidateTLD(args[0])
		if err != nil {
			return err
		}
		if tld == "dev" || tld == "app" {
			fmt.Printf("warning: .%s is HSTS-preloaded — browsers will force HTTPS on it\n", tld)
		}
		if err := a.SetTLD(tld); err != nil {
			return err
		}
		fmt.Printf("TLD is now .%s — sites are served as <name>.%s\n", tld, tld)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(tldCmd)
}
