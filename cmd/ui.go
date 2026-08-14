package cmd

import (
	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/ui"
)

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open the Mullion control panel window",
	Long: `Opens Mullion's control panel in an app-mode browser window: service
status, PHP versions, MySQL, and the linked sites — all clickable.
Double-clicking mullion.exe opens the same panel once setup has been run.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if a, err := app.New(); err == nil && a.State.Config.GlobalPHP != "" {
			selfUpdateIfNeeded(a)
		}
		return ui.Run(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(uiCmd)
}
