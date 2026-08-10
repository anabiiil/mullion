package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/phpver"
)

var isolateSite string

var isolateCmd = &cobra.Command{
	Use:   "isolate <version>",
	Short: "Pin the current project's site to a specific PHP version",
	Long: `Makes one site run on a different PHP version than the system default,
e.g. an old client project on PHP 7.4 while everything else uses 8.3.`,
	Args: cobra.ExactArgs(1),
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
		name, err := targetSiteName(cmd, a)
		if err != nil {
			return err
		}
		site := a.State.FindSite(name)
		if site == nil {
			return fmt.Errorf("no site named %q", name)
		}
		site.PHP = full
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s now runs on PHP %s\n", a.State.Host(*site), full)
		return nil
	},
}

var unisolateCmd = &cobra.Command{
	Use:   "unisolate",
	Short: "Make the current project's site follow the global PHP version again",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		name, err := targetSiteName(cmd, a)
		if err != nil {
			return err
		}
		site := a.State.FindSite(name)
		if site == nil {
			return fmt.Errorf("no site named %q", name)
		}
		site.PHP = ""
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s now follows the global PHP version (%s)\n",
			a.State.Host(*site), a.State.Config.GlobalPHP)
		return nil
	},
}

// targetSiteName picks the site for isolate/unisolate: the --site flag
// when given, otherwise the site linked to the current directory.
func targetSiteName(cmd *cobra.Command, a *app.App) (string, error) {
	if isolateSite != "" {
		return slugify(isolateSite), nil
	}
	return siteNameFromArgOrCwd(a, nil)
}

func init() {
	isolateCmd.Flags().StringVar(&isolateSite, "site", "", "site name (defaults to the current directory's link)")
	unisolateCmd.Flags().StringVar(&isolateSite, "site", "", "site name (defaults to the current directory's link)")
	rootCmd.AddCommand(isolateCmd, unisolateCmd)
}
