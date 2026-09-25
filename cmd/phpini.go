package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/phpver"
)

var phpIniCmd = &cobra.Command{
	Use:   "ini [version]",
	Short: "Show the version's curated php.ini settings; defaults to the global version",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version, err := extTargetVersion(a, args, 0)
		if err != nil {
			return err
		}
		settings, err := phpver.ListIni(a.Paths, version)
		if err != nil {
			return err
		}
		fmt.Printf("PHP %s php.ini settings:\n", version)
		for _, s := range settings {
			fmt.Printf("  %s = %s\n", s.Key, s.Value)
		}
		fmt.Printf("\nChange with: mullion php ini set <key> <value> [%s]\n", version)
		return nil
	},
}

var phpIniSetCmd = &cobra.Command{
	Use:   "set <key> <value> [version]",
	Short: "Set a curated php.ini setting (e.g. memory_limit, display_errors) and restart PHP",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		version, err := extTargetVersion(a, args, 2)
		if err != nil {
			return err
		}
		if err := phpver.SetIni(a.Paths, version, args[0], args[1]); err != nil {
			return err
		}
		// Restart the version's php-fpm/php-cgi so running sites see the change.
		if err := a.RestartPhp(version); err != nil {
			return err
		}
		fmt.Printf("%s = %s for PHP %s.\n", args[0], args[1], version)
		return nil
	},
}

func init() {
	phpIniCmd.AddCommand(phpIniSetCmd)
	phpCmd.AddCommand(phpIniCmd)
}
