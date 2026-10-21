package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
)

var adminerCmd = &cobra.Command{
	Use:   "adminer",
	Short: "Adminer — SQL Server's web admin tool, at https://adminer.<tld>",
	Long: `Downloads the latest Adminer (a single-file PHP database UI) from its
official GitHub releases into ~/.mullion/adminer and serves it as a secured
site at https://adminer.<tld>. Its SQL Server login needs PHP's sqlsrv or
pdo_sqlsrv extension (PHP → Extensions in the panel).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return ensureAdminer(cmd, mustApp(), false)
	},
}

var adminerInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Adminer and link it at https://adminer.<tld>",
	RunE: func(cmd *cobra.Command, args []string) error {
		return ensureAdminer(cmd, mustApp(), false)
	},
}

var adminerOpenCmd = &cobra.Command{
	Use:   "open",
	Short: "Open Adminer on the SQL Server login (installing it first if needed)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return ensureAdminer(cmd, mustApp(), true)
	},
}

func ensureAdminer(cmd *cobra.Command, a *app.App, open bool) error {
	url, err := a.EnsureAdminer(cmd.Context())
	if err != nil {
		return err
	}
	fmt.Println("Adminer is ready:", url)
	if a.State.Config.MSSQL != "" && a.SQLServerPHPExtension() == "" {
		fmt.Println("note: " + app.PHPExtensionHint)
	}
	if open {
		openURL(url)
	}
	return nil
}

var adminerUninstallYes bool

var adminerUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove Adminer (no database is touched)",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		proceed, err := confirmDestructive(adminerUninstallYes, "Remove Adminer?")
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Println("Aborted.")
			return nil
		}
		if err := a.UninstallAdminTool("adminer"); err != nil {
			return err
		}
		fmt.Println("Adminer removed.")
		return nil
	},
}

func init() {
	adminerUninstallCmd.Flags().BoolVar(&adminerUninstallYes, "yes", false, "do not ask for confirmation")
	adminerCmd.AddCommand(adminerInstallCmd, adminerOpenCmd, adminerUninstallCmd)
	rootCmd.AddCommand(adminerCmd)
}
