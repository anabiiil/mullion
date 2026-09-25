package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var composerCmd = &cobra.Command{
	Use:   "composer",
	Short: "Manage the Composer installation",
}

var composerInstallCmd = &cobra.Command{
	Use:   "install [version]",
	Short: "Install Composer (latest by default, or a specific version like 2.2.25)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version := ""
		if len(args) == 1 {
			version = args[0]
		}
		installed, err := a.InstallComposer(cmd.Context(), version)
		if err != nil {
			return err
		}
		fmt.Printf("Composer (%s) installed — available as `composer` in any new terminal.\n", installed)
		return nil
	},
}

func init() {
	composerCmd.AddCommand(composerInstallCmd)
	rootCmd.AddCommand(composerCmd)
}
