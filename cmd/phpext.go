package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/phpver"
)

var phpExtCmd = &cobra.Command{
	Use:   "ext",
	Short: "Manage the extensions of an installed PHP version",
}

var phpExtListCmd = &cobra.Command{
	Use:   "list [version]",
	Short: "List the version's extensions ([x] = enabled); defaults to the global version",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version, err := extTargetVersion(a, args, 0)
		if err != nil {
			return err
		}
		exts, err := phpver.ListExtensions(a.Paths, version)
		if err != nil {
			return err
		}
		fmt.Printf("PHP %s extensions:\n", version)
		for _, e := range exts {
			mark := "[ ]"
			if e.Enabled {
				mark = "[x]"
			}
			fmt.Printf("  %s %s\n", mark, e.Name)
		}
		fmt.Printf("\nToggle with: mullion php ext enable|disable <name> [%s]\n", version)
		return nil
	},
}

var phpExtEnableCmd = &cobra.Command{
	Use:   "enable <name> [version]",
	Short: "Enable an extension (e.g. intl, soap, ldap)",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  func(cmd *cobra.Command, args []string) error { return setExt(args, true) },
}

var phpExtDisableCmd = &cobra.Command{
	Use:   "disable <name> [version]",
	Short: "Disable an extension",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  func(cmd *cobra.Command, args []string) error { return setExt(args, false) },
}

func setExt(args []string, enable bool) error {
	a := mustApp()
	version, err := extTargetVersion(a, args, 1)
	if err != nil {
		return err
	}
	if err := phpver.SetExtension(a.Paths, version, args[0], enable); err != nil {
		return err
	}
	// Restart the version's php-cgi so running sites see the change.
	if err := a.RestartPhp(version); err != nil {
		return err
	}
	state := "enabled"
	if !enable {
		state = "disabled"
	}
	fmt.Printf("%s %s for PHP %s.\n", args[0], state, version)
	return nil
}

// extTargetVersion picks the version from args[idx] (a selector like
// "8.4" is fine), falling back to the global version.
func extTargetVersion(a *app.App, args []string, idx int) (string, error) {
	if len(args) > idx {
		sel, err := phpver.ParseSelector(args[idx])
		if err != nil {
			return "", err
		}
		return phpver.FindInstalled(a.Paths, sel)
	}
	if a.State.Config.GlobalPHP == "" {
		return "", fmt.Errorf("no global PHP version set (run `mullion use <version>` or pass a version)")
	}
	return a.State.Config.GlobalPHP, nil
}

func init() {
	phpExtCmd.AddCommand(phpExtListCmd, phpExtEnableCmd, phpExtDisableCmd)
	phpCmd.AddCommand(phpExtCmd)
}
