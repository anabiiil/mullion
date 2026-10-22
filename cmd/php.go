package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/console"
	"pm/internal/phpver"
)

var phpCmd = &cobra.Command{
	Use:   "php",
	Short: "Manage installed PHP versions",
}

var phpAvailableCmd = &cobra.Command{
	Use:   "available",
	Short: "List PHP versions available to install (current branches)",
	RunE: func(cmd *cobra.Command, args []string) error {
		releases, err := phpver.FetchCurrent(cmd.Context())
		if err != nil {
			return err
		}
		fmt.Println("Current releases on " + phpver.Source + ":")
		for _, r := range releases {
			fmt.Printf("  %s\n", r.Version)
		}
		if runtime.GOOS == "windows" {
			fmt.Println("\nOlder (end-of-life) versions can also be installed by number, e.g.: mullion php install 7.4")
		} else {
			fmt.Println("\nOlder patch releases (PHP 8.0 and newer) can also be installed by number, e.g.: mullion php install 8.1")
		}
		return nil
	},
}

var phpInstallCmd = &cobra.Command{
	Use:   "install <version>",
	Short: "Download and install a PHP version (e.g. 8.3 or 8.3.26)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		sel, err := phpver.ParseSelector(args[0])
		if err != nil {
			return err
		}
		rel, err := phpver.Resolve(cmd.Context(), sel)
		if err != nil {
			return err
		}
		// "8.4" while an older 8.4.x is installed: that's an update (one
		// 8.4 with the sites moved over), not a second 8.4 side by side.
		if older := olderSameBranch(a, sel, rel.Version); older != "" {
			fmt.Printf("PHP %s is installed; %s is the newest %s.\n", older, rel.Version, phpver.Branch(rel.Version))
			if console.Interactive() && askYesNo(fmt.Sprintf("Update %s to %s instead (moves its sites and settings over, removes %s)?", older, rel.Version, older), true) {
				return runPhpUpdate(cmd.Context(), a, []string{older}, false)
			}
			fmt.Printf("Installing %s alongside it. (To update instead: mullion php update %s)\n", rel.Version, phpver.Branch(rel.Version))
		}
		dir, err := phpver.Install(cmd.Context(), a.Paths, rel)
		if err != nil {
			return err
		}
		fmt.Printf("PHP %s installed at %s\n", rel.Version, dir)
		if a.State.Config.GlobalPHP == "" {
			fmt.Printf("Tip: make it the default with: mullion use %s\n", rel.Version)
		}
		return nil
	},
}

var phpListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed PHP versions",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		versions, err := phpver.Installed(a.Paths)
		if err != nil {
			return err
		}
		if len(versions) == 0 {
			fmt.Println("No PHP versions installed yet. Try: mullion php install 8.3")
			return nil
		}
		// Update hints come from the cached release listing (refreshed at
		// most every 6 h); offline they are simply left out.
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		ups, _ := a.PHPUpdates(ctx, false)
		cancel()
		for _, v := range versions {
			marker := "  "
			if v == a.State.Config.GlobalPHP {
				marker = "* "
			}
			fmt.Println(marker + v + updateHint(ups[v]))
		}
		if len(ups) > 0 {
			fmt.Println("\nUpdate with: mullion php update   (or one branch: mullion php update 8.4)")
		}
		return nil
	},
}

// updateHint is `php list`'s "(update: 8.4.27)" note.
func updateHint(u phpver.Update) string {
	if u.To == "" {
		return ""
	}
	if u.Security {
		return "  (update: " + u.To + ", security)"
	}
	return "  (update: " + u.To + ")"
}

// olderSameBranch returns the newest installed version of rel's branch
// when it is older than rel and the user asked for the branch, not an
// exact patch ("" otherwise).
func olderSameBranch(a *app.App, sel phpver.Selector, latest string) string {
	if sel.Patch != -1 || sel.Minor == -1 {
		return ""
	}
	installed, err := phpver.FindInstalled(a.Paths, phpver.Selector{Major: sel.Major, Minor: sel.Minor, Patch: -1})
	if err != nil || phpver.Compare(installed, latest) >= 0 {
		return ""
	}
	return installed
}

// phpUpdateCheckExitCode is `mullion php update --check`'s exit status
// when at least one installed version has a newer patch (0: none).
const phpUpdateCheckExitCode = 10

var (
	phpUpdateAll     bool
	phpUpdateKeepOld bool
	phpUpdateCheck   bool
)

var phpUpdateCmd = &cobra.Command{
	Use:   "update [<branch|version>]",
	Short: "Update installed PHP to the newest patch of its branch (e.g. 8.4.23 -> 8.4.27)",
	Long: `Installs the newest patch release of an installed branch and moves everything
over to it: php.ini (extensions, OPcache, limits, Mail's sendmail_path — paths into
the old version's folder are re-pointed; on macOS LDAP / SQL Server modules are
rebuilt for the new patch), the global version, sites pinned to the old patch,
and their workers. The old patch is removed afterwards unless --keep-old.
Nothing is switched if the new version fails to install or start.

  mullion php update            update every installed version that has a newer patch
  mullion php update 8.4        update the installed 8.4.x
  mullion php update --check    only check: exit 0 when up to date, 10 when updates exist`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if phpUpdateAll && len(args) > 0 {
			return errors.New("give a version or --all, not both")
		}
		if len(args) > 0 {
			sel, err := phpver.ParseSelector(args[0])
			if err != nil {
				return err
			}
			if _, err := phpver.FindInstalled(a.Paths, sel); err != nil {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
		ups, err := a.PHPUpdates(ctx, true)
		cancel()
		if err != nil {
			return err
		}
		targets, err := selectPhpUpdates(ups, args)
		if err != nil {
			return err
		}
		if phpUpdateCheck {
			printPhpUpdates(ups, targets)
			if code := phpUpdateCheckCode(targets); code != 0 {
				os.Exit(code)
			}
			return nil
		}
		if len(targets) == 0 {
			fmt.Println("PHP is up to date — every installed version is the newest patch of its branch.")
			return nil
		}
		return runPhpUpdate(cmd.Context(), a, targets, true)
	},
}

// selectPhpUpdates picks the installed versions to update: all outdated
// ones without an argument, else those matching it ("8.4", "8.4.23").
func selectPhpUpdates(ups map[string]phpver.Update, args []string) ([]string, error) {
	var out []string
	if len(args) == 0 {
		for v := range ups {
			out = append(out, v)
		}
	} else {
		sel, err := phpver.ParseSelector(args[0])
		if err != nil {
			return nil, err
		}
		if sel.Minor == -1 {
			return nil, fmt.Errorf("say which branch to update, e.g. %d.4 (an update stays within one major.minor)", sel.Major)
		}
		for v := range ups {
			if sel.Matches(v) {
				out = append(out, v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return phpver.Compare(out[i], out[j]) < 0 })
	return out, nil
}

// phpUpdateCheckCode is the exit status of --check for the selected updates.
func phpUpdateCheckCode(targets []string) int {
	if len(targets) > 0 {
		return phpUpdateCheckExitCode
	}
	return 0
}

func printPhpUpdates(ups map[string]phpver.Update, targets []string) {
	if len(targets) == 0 {
		fmt.Println("PHP is up to date.")
		return
	}
	for _, v := range targets {
		u := ups[v]
		sec := ""
		if u.Security {
			sec = "  (security release)"
		}
		fmt.Printf("  %s -> %s%s\n", u.From, u.To, sec)
	}
}

// runPhpUpdate updates each version in turn (asking first in a
// terminal when confirm is set), printing the steps.
func runPhpUpdate(ctx context.Context, a *app.App, versions []string, confirm bool) error {
	if confirm && console.Interactive() {
		for _, v := range versions {
			fmt.Println(describePhpUpdatePlan(a, v))
		}
		if !askYesNo("Update now?", true) {
			return nil
		}
	}
	opts := app.PHPUpdateOptions{KeepOld: phpUpdateKeepOld, Log: func(s string) { fmt.Println("==> " + s) }}
	for _, v := range versions {
		res, err := a.UpdatePHP(ctx, v, opts)
		if errors.Is(err, app.ErrPHPUpToDate) {
			fmt.Printf("PHP %s is already the newest patch.\n", v)
			continue
		}
		if err != nil {
			return err
		}
		for _, w := range res.Warnings {
			fmt.Println("warning: " + w)
		}
		fmt.Printf("PHP %s is now %s.\n", res.From, res.To)
	}
	return nil
}

// describePhpUpdatePlan is the "what will switch" summary for one version.
func describePhpUpdatePlan(a *app.App, v string) string {
	ups, _ := a.PHPUpdates(context.Background(), false)
	to := ups[v].To
	if to == "" {
		return "PHP " + v
	}
	p := a.PlanPHPUpdate(v, to)
	s := fmt.Sprintf("PHP %s -> %s", v, to)
	if ups[v].Security {
		s += " (security release)"
	}
	var bits []string
	if p.Global {
		bits = append(bits, fmt.Sprintf("the global version (%d site(s) following it)", p.Following))
	}
	if len(p.Sites) > 0 {
		bits = append(bits, fmt.Sprintf("%d pinned site(s) %v", len(p.Sites), p.Sites))
	}
	if p.Workers > 0 {
		bits = append(bits, fmt.Sprintf("%d worker(s)", p.Workers))
	}
	if len(bits) > 0 {
		s += "\n  switches: " + strings.Join(bits, ", ")
	}
	if phpUpdateKeepOld {
		s += "\n  keeps " + v + " installed"
	} else {
		s += "\n  removes " + v + " afterwards (--keep-old to keep it)"
	}
	return s
}

var phpUninstallCmd = &cobra.Command{
	Use:   "uninstall <version>",
	Short: "Remove an installed PHP version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mustApp().UninstallPhp(args[0])
	},
}

func init() {
	phpUpdateCmd.Flags().BoolVar(&phpUpdateAll, "all", false, "update every installed version that has a newer patch (the default without a version)")
	phpUpdateCmd.Flags().BoolVar(&phpUpdateKeepOld, "keep-old", false, "keep the old patch installed after switching")
	phpUpdateCmd.Flags().BoolVar(&phpUpdateCheck, "check", false, "only check: exit 0 when up to date, 10 when updates exist")
	phpCmd.AddCommand(phpAvailableCmd, phpInstallCmd, phpListCmd, phpUninstallCmd, phpUpdateCmd)
	rootCmd.AddCommand(phpCmd)
}
