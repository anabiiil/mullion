package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/proc"
	"pm/internal/ui"
)

var uiDetached bool

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open the Mullion control panel window",
	Long: `Opens Mullion's control panel in an app-mode browser window: service
status, PHP versions, Node, MySQL, and the linked sites — all clickable.
The panel runs in the background: closing this terminal does not close it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if a, err := app.New(); err == nil && a.State.Config.GlobalPHP != "" {
			selfUpdateIfNeeded(a)
		}
		// Hand the panel to a background process so the terminal is free
		// (and closing it doesn't kill the panel). The child carries the
		// flag so it doesn't re-detach.
		if !uiDetached && runtime.GOOS != "windows" {
			if exe, err := os.Executable(); err == nil {
				c := proc.Quiet(exe, "ui", "--detached")
				proc.Detach(c)
				if err := c.Start(); err == nil {
					_ = c.Process.Release()
					fmt.Println("Control panel opening — this terminal is free (the panel keeps running in the background).")
					return nil
				}
			}
		}
		return ui.Run(cmd.Context())
	},
}

func init() {
	uiCmd.Flags().BoolVar(&uiDetached, "detached", false, "internal: already detached from the terminal")
	_ = uiCmd.Flags().MarkHidden("detached")
	rootCmd.AddCommand(uiCmd)
}
