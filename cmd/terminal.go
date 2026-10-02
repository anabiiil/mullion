package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"pm/internal/mterm"
	"pm/internal/term"
)

var terminalCmd = &cobra.Command{
	Use:   "terminal [path|site]",
	Short: "Open a folder or site in Mullion Terminal",
	Long: `Opens the folder (default: the current one) or a linked site's folder
as a new tab in Mullion Terminal — the standalone terminal app — when
it is your chosen terminal (Settings → Terminal in the control panel).
With the built-in terminal chosen, use the panel's Terminal page.

  mullion terminal install   install or update Mullion Terminal`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		if len(args) == 1 {
			if info, err := os.Stat(args[0]); err == nil && info.IsDir() {
				dir, _ = filepath.Abs(args[0])
			} else if s := a.State.FindSite(args[0]); s != nil {
				dir = s.Path
			} else {
				return fmt.Errorf("%q is neither a folder nor a linked site", args[0])
			}
		}
		_, installed := mterm.Find()
		if a.State.Config.UI["terminal.app"] != "external" {
			fmt.Println("Your terminal is the control panel's built-in one: run `mullion ui` and open the Terminal page.")
			if installed {
				fmt.Println("To open folders in Mullion Terminal instead, choose it in Settings → Terminal.")
			} else if mterm.Available() {
				fmt.Println("For the standalone Mullion Terminal app: mullion terminal install")
			}
			return nil
		}
		if !installed {
			return fmt.Errorf("Mullion Terminal is your chosen terminal but isn't installed — run: mullion terminal install")
		}
		if err := mterm.Open(dir); err != nil {
			return err
		}
		fmt.Println("Opened", dir, "in Mullion Terminal.")
		return nil
	},
}

var terminalInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install or update Mullion Terminal and make it your terminal",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !mterm.Available() {
			return fmt.Errorf("%s", mterm.UnavailableReason())
		}
		a := mustApp()
		fmt.Println("Installing the latest Mullion Terminal…")
		v, err := mterm.Install(cmd.Context(), func(stage string, done, total int64) {
			switch {
			case stage == "install":
				fmt.Printf("\r  Installing…%s\n", term.ClearLine())
			case total > 0:
				fmt.Printf("\r  Downloading %3d%%  %.1f / %.1f MB%s", done*100/total, float64(done)/1e6, float64(total)/1e6, term.ClearLine())
			default:
				fmt.Printf("\r  Downloading %.1f MB%s", float64(done)/1e6, term.ClearLine())
			}
		})
		if err != nil {
			fmt.Println()
			return err
		}
		// Installing it on purpose is the answer to the panel's
		// "Choose your terminal" question.
		if a.State.Config.UI == nil {
			a.State.Config.UI = map[string]string{}
		}
		a.State.Config.UI["terminal.app"] = "external"
		a.State.Config.UI["terminal.appChosen"] = "1"
		if err := a.State.Save(); err != nil {
			return err
		}
		p, _ := mterm.Find()
		fmt.Printf("%s Mullion Terminal %s installed at %s — it's now your terminal (change it in Settings → Terminal).\n", term.Green("✓"), v, p)
		return nil
	},
}

func init() {
	terminalCmd.AddCommand(terminalInstallCmd)
	rootCmd.AddCommand(terminalCmd)
}
