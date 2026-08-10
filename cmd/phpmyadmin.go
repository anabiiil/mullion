package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/caddy"
	"pm/internal/config"
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
// converges the machine. Shared by `mullion phpmyadmin` and `mullion setup`.
func ensurePhpMyAdmin(ctx context.Context, a *app.App, version string) error {
	if a.State.Config.GlobalPHP == "" {
		return fmt.Errorf("phpMyAdmin needs PHP: run `mullion php install 8.4` and `mullion use 8.4` first")
	}

	if err := phpmyadmin.Install(ctx, a.Paths, version); err != nil {
		return err
	}
	if site := a.State.FindSite("phpmyadmin"); site == nil {
		a.State.AddSite(config.Site{Name: "phpmyadmin", Path: a.Paths.PhpMyAdminDir(), Secure: true})
	} else {
		site.Secure = true
	}
	if err := a.Apply(); err != nil {
		return err
	}
	// Make sure Caddy's local root CA is in the system trust store so
	// browsers show the padlock (one-time confirmation prompt).
	if err := caddy.TrustCA(a.Paths); err != nil {
		fmt.Println("note:", err)
		fmt.Println("If the browser warns about the certificate, run `mullion start` again as administrator once.")
	}

	fmt.Printf("phpMyAdmin is ready: https://phpmyadmin.%s\n", a.State.Config.TLD)
	return nil
}

func init() {
	rootCmd.AddCommand(phpmyadminCmd)
}
