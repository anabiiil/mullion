package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/proc"
	"pm/internal/selfupdate"
	"pm/internal/term"
	"pm/internal/tty"
)

// upgradeCheckExitCode is `mullion upgrade --check`'s exit status when
// a newer release exists (0 means up to date).
const upgradeCheckExitCode = 10

var (
	upgradeCheck       bool
	upgradeYes         bool
	upgradeFinish      bool
	upgradeFrom        string
	upgradePanelPID    int
	upgradeReopenPanel bool
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade Mullion to the latest release",
	Long: `Checks GitHub for a newer Mullion release and upgrades through the way
Mullion was installed: Homebrew (brew upgrade anabiiil/tap/mullion),
Scoop (scoop update mullion), or — for a downloaded binary — by
downloading the release and swapping the executable in place. The
background agent, the Windows tray and an open control panel restart
on the new version (services blip for a moment).

  mullion upgrade          upgrade (asks first when run in a terminal)
  mullion upgrade --check  only check: exit 0 when up to date, 10 when an update exists`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if upgradeFinish {
			return finishUpgrade()
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
		info := selfupdate.Check(ctx, true)
		cancel()
		printUpgradeInfo(info)
		if info.Error != "" {
			return errors.New(info.Error)
		}
		if upgradeCheck {
			if info.Update {
				os.Exit(upgradeCheckExitCode)
			}
			return nil
		}
		if !info.Update {
			fmt.Println(term.Green("✓ Mullion is up to date."))
			return nil
		}
		if !upgradeYes && tty.IsTerminal(os.Stdin) &&
			!askYesNo(fmt.Sprintf("Upgrade to v%s now? Mullion's services restart briefly.", info.Latest), true) {
			return nil
		}

		res, err := selfupdate.Upgrade(cmd.Context(), selfupdate.Reporter{
			Line: func(s string) { fmt.Println(s) },
		})
		if errors.Is(err, selfupdate.ErrUpToDate) {
			fmt.Println(term.Green("✓ Mullion is up to date."))
			return nil
		}
		if err != nil {
			return err
		}

		// The relaunch must run the NEW code: hand it to the new binary.
		fmt.Println("==> Restarting Mullion on v" + res.To + "…")
		c := proc.Quiet(res.Exe, "upgrade", "--finish", "--from", res.From)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		c.Env = append(os.Environ(), "MULLION_NO_REDIRECT=1")
		if err := c.Run(); err != nil {
			fmt.Println("note: restarting Mullion's background pieces failed -", err, "— run `mullion start` to finish")
		}
		fmt.Println(term.Green("✓ Mullion is now v" + res.To + "."))
		return nil
	},
}

func printUpgradeInfo(info selfupdate.Info) {
	fmt.Printf("Current:  v%s (%s)\n", info.Current, info.ChannelLabel)
	switch {
	case info.Error != "":
		fmt.Printf("Latest:   %s\n", term.Yellow("unknown — "+info.Error))
	case info.Update:
		date := ""
		if !info.Published.IsZero() {
			date = " · released " + info.Published.Local().Format("2006-01-02")
		}
		fmt.Printf("Latest:   %s%s\n", term.Green("v"+info.Latest), date)
		if info.URL != "" {
			fmt.Printf("What's new: %s\n", info.URL)
		}
	default:
		fmt.Printf("Latest:   v%s\n", info.Latest)
	}
}

// finishUpgrade is the second half of an upgrade, run by the NEW binary
// (`upgrade --finish`, hidden): refresh the install and move the agent,
// tray and panel onto this version. The panel launches it detached, so
// it first gives the page a moment to read the finished job.
func finishUpgrade() error {
	if upgradePanelPID > 0 {
		time.Sleep(2 * time.Second)
	}
	a, err := app.New()
	if err != nil {
		return err
	}
	exe, _ := os.Executable()
	return selfupdate.Relaunch(selfupdate.RelaunchOptions{
		Paths: a.Paths,
		Exe:   exe,
		SelfUpdate: func() {
			// Same guard as `mullion ui`: only a machine that was set up
			// has an install to refresh.
			if _, err := os.Stat(a.Paths.VersionFile()); err == nil || a.State.Config.GlobalPHP != "" {
				selfUpdateIfNeeded(a)
			}
		},
		PanelPID:    upgradePanelPID,
		ReopenPanel: upgradeReopenPanel,
		Log:         func(s string) { fmt.Println(s) },
	})
}

func init() {
	upgradeCmd.Flags().BoolVar(&upgradeCheck, "check", false, "only check for a newer release (exit code 10 when one exists)")
	upgradeCmd.Flags().BoolVarP(&upgradeYes, "yes", "y", false, "do not ask for confirmation")
	upgradeCmd.Flags().BoolVar(&upgradeFinish, "finish", false, "internal: relaunch Mullion on this (new) binary")
	upgradeCmd.Flags().StringVar(&upgradeFrom, "from", "", "internal: the version upgraded from")
	upgradeCmd.Flags().IntVar(&upgradePanelPID, "panel-pid", 0, "internal: the old panel process to replace")
	upgradeCmd.Flags().BoolVar(&upgradeReopenPanel, "reopen-panel", false, "internal: reopen the control panel afterwards")
	for _, f := range []string{"finish", "from", "panel-pid", "reopen-panel"} {
		_ = upgradeCmd.Flags().MarkHidden(f)
	}
	rootCmd.AddCommand(upgradeCmd)
}
