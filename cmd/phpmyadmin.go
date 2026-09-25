package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/phpmyadmin"
)

var phpmyadminCmd = &cobra.Command{
	Use:   "phpmyadmin [version]",
	Short: "Install phpMyAdmin and serve it at https://phpmyadmin.<tld>",
	Long: `Downloads phpMyAdmin (the latest release, or a specific version like
5.2.2) into ~/.mullion/phpmyadmin, configures it to auto-connect to the local
MySQL server (root, no password), and links it as a secured site at
https://phpmyadmin.<tld>.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version := ""
		if len(args) == 1 {
			version = args[0]
		}
		if err := ensurePhpMyAdmin(cmd.Context(), a, version); err != nil {
			return err
		}
		if a.State.Config.MySQL == "" {
			fmt.Println("note: MySQL is not installed yet — run `mullion mysql install` so phpMyAdmin has a server to connect to.")
		}
		return nil
	},
}

// ensurePhpMyAdmin installs phpMyAdmin, links it as a secured site, and
// converges the machine. Shared by `mullion phpmyadmin` and `mullion setup`;
// the reusable work lives in internal/phpmyadmin.EnsureLinked so the
// control panel can call it too without importing cmd.
func ensurePhpMyAdmin(ctx context.Context, a *app.App, version string) error {
	url, err := phpmyadmin.EnsureLinked(ctx, a, version)
	if err != nil {
		return err
	}
	fmt.Printf("phpMyAdmin is ready: %s\n", url)
	return nil
}

var phpmyadminUninstallYes bool

var phpmyadminUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove phpMyAdmin (the MySQL server itself is untouched)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		proceed, err := confirmDestructive(phpmyadminUninstallYes, "Remove phpMyAdmin?")
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		if err := a.UninstallAdminTool("phpmyadmin"); err != nil {
			return err
		}
		fmt.Println("phpMyAdmin removed.")
		return nil
	},
}

func init() {
	phpmyadminUninstallCmd.Flags().BoolVar(&phpmyadminUninstallYes, "yes", false, "do not ask for confirmation")
	phpmyadminCmd.AddCommand(phpmyadminUninstallCmd)
	rootCmd.AddCommand(phpmyadminCmd)
}
