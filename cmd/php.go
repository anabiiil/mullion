package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

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
		for _, v := range versions {
			marker := "  "
			if v == a.State.Config.GlobalPHP {
				marker = "* "
			}
			fmt.Println(marker + v)
		}
		return nil
	},
}

var phpUninstallCmd = &cobra.Command{
	Use:   "uninstall <version>",
	Short: "Remove an installed PHP version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		sel, err := phpver.ParseSelector(args[0])
		if err != nil {
			return err
		}
		full, err := phpver.FindInstalled(a.Paths, sel)
		if err != nil {
			return err
		}
		if full == a.State.Config.GlobalPHP {
			return fmt.Errorf("PHP %s is the active global version; switch first with `mullion use <other>`", full)
		}
		for _, s := range a.State.Sites {
			if s.PHP == full {
				return fmt.Errorf("site %q is isolated to PHP %s; run `mullion unisolate` there first", s.Name, full)
			}
		}
		if err := os.RemoveAll(a.Paths.PhpVersionDir(full)); err != nil {
			return err
		}
		fmt.Println("Removed PHP", full)
		return nil
	},
}

func init() {
	phpCmd.AddCommand(phpAvailableCmd, phpInstallCmd, phpListCmd, phpUninstallCmd)
	rootCmd.AddCommand(phpCmd)
}
