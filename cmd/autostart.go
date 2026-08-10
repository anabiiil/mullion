package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/autostart"
)

var autostartCmd = &cobra.Command{
	Use:   "autostart [on|off]",
	Short: "Start Mullion automatically when you sign in to Windows",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if len(args) == 0 {
			if autostart.Enabled() {
				fmt.Println("autostart: on")
			} else {
				fmt.Println("autostart: off (enable with `mullion autostart on`)")
			}
			return nil
		}
		switch strings.ToLower(args[0]) {
		case "on":
			if err := autostart.Enable(a.Paths); err != nil {
				return err
			}
			fmt.Println("Mullion will start automatically when you sign in to Windows.")
		case "off":
			if err := autostart.Disable(); err != nil {
				return err
			}
			fmt.Println("Autostart disabled.")
		default:
			return fmt.Errorf("expected `on` or `off`, got %q", args[0])
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(autostartCmd)
}
