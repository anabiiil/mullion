package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/caddy"
	"pm/internal/mongoexpress"
)

var mongoExpressCmd = &cobra.Command{
	Use:     "mongo-express",
	Aliases: []string{"mongo-ui"},
	Short:   "Install mongo-express and serve it at https://mongo.<tld>",
	Long: `Installs mongo-express (a web-based admin UI for MongoDB) into
~/.mullion/mongo-express, pointed at mullion's local MongoDB with admin
access and no basic-auth prompt, and links it as a secured site at
https://mongo.<tld>.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		url, err := mongoexpress.Ensure(cmd.Context(), a)
		if err != nil {
			return err
		}
		// Make sure Caddy's local root CA is in the system trust store so
		// browsers show the padlock (one-time confirmation prompt).
		if err := caddy.TrustCA(a.Paths); err != nil {
			fmt.Println("note:", err)
			fmt.Println("If the browser warns about the certificate, run `mullion start` again as administrator once.")
		}
		fmt.Println("mongo-express is ready:", url)
		return nil
	},
}

var mongoExpressUninstallYes bool

var mongoExpressUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove mongo-express (the MongoDB server itself is untouched)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		proceed, err := confirmDestructive(mongoExpressUninstallYes, "Remove mongo-express?")
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		if err := a.UninstallAdminTool("mongo-express"); err != nil {
			return err
		}
		fmt.Println("mongo-express removed.")
		return nil
	},
}

func init() {
	mongoExpressUninstallCmd.Flags().BoolVar(&mongoExpressUninstallYes, "yes", false, "do not ask for confirmation")
	mongoExpressCmd.AddCommand(mongoExpressUninstallCmd)
	rootCmd.AddCommand(mongoExpressCmd)
}
