package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/proc"
	"pm/internal/ui"
)

var uiDetached bool
var uiWindowHost bool

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open the Mullion control panel window",
	Long: `Opens Mullion's control panel in an app-mode browser window: service
status, PHP versions, Node, MySQL, and the linked sites — all clickable.
The panel runs in the background: closing this terminal does not close it.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if uiWindowHost {
			// Mullion.app runs us as a child process and owns our
			// lifetime via stdin — no self-update chatter, no
			// detaching, no window/tab of our own. cobra doesn't wire
			// cmd.Context() to signals, so handle them here.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return ui.RunHost(ctx, os.Stdin, os.Stdout)
		}
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
	uiCmd.Flags().BoolVar(&uiWindowHost, "window-host", false, "internal: serve the panel for the native app host (Mullion.app)")
	_ = uiCmd.Flags().MarkHidden("window-host")
	rootCmd.AddCommand(uiCmd)
}
