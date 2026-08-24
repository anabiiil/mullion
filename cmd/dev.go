package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/devserver"
)

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Control the managed dev servers of frontend sites",
	Long: `Each linked frontend site gets a managed dev server (npm run dev)
behind its .test domain. It starts with ` + "`mullion start`" + ` and stays up in
the background. Stop one here when you don't need it — it stays stopped
(and its domain shows a friendly hint) until you start it again.`,
}

var devStartCmd = &cobra.Command{
	Use:   "start [name]",
	Short: "Start (and unpause) a site's dev server",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, site, err := devTarget(a2(), args)
		if err != nil {
			return err
		}
		site.DevPaused = false
		site.Mode = "dev"
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s is serving the dev server.\n", a.State.Host(*site))
		return nil
	},
}

var devStopCmd = &cobra.Command{
	Use:   "stop [name]",
	Short: "Stop a site's dev server and keep it stopped (frees RAM/CPU)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, site, err := devTarget(a2(), args)
		if err != nil {
			return err
		}
		site.DevPaused = true
		devserver.Stop(a.Paths, site.Name)
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s's dev server is stopped and will stay stopped (start again with `mullion dev start %s`).\n",
			a.State.Host(*site), site.Name)
		return nil
	},
}

var devRestartCmd = &cobra.Command{
	Use:   "restart [name]",
	Short: "Restart a site's dev server (picks up config/env changes)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, site, err := devTarget(a2(), args)
		if err != nil {
			return err
		}
		site.DevPaused = false
		devserver.Stop(a.Paths, site.Name)
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s's dev server restarted.\n", a.State.Host(*site))
		return nil
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve <dev|build> [name]",
	Short: "Choose what a frontend site's domain serves: the dev server, or the last production build",
	Long: `Switches one frontend domain between two modes:

  dev     the managed dev server (live code, HMR)
  build   the last production build (dist/, build/, out/) served statically

With "build" the dev server is stopped too — the domain costs nothing.
Great for checking what's actually deployed, or freeing resources.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := a2()
		mode := args[0]
		if mode != "dev" && mode != "build" {
			return fmt.Errorf("expected `dev` or `build`, got %q", mode)
		}
		_, site, err := devTarget(a, args[1:])
		if err != nil {
			return err
		}
		if mode == "build" {
			buildDir, err := a.EnsureBuildOutput(site.Path, site.BuildDir)
			if err != nil {
				return err
			}
			site.BuildDir = buildDir
			site.Mode = "build"
			devserver.Stop(a.Paths, site.Name)
		} else {
			site.Mode = "dev"
			site.DevPaused = false
		}
		if err := a.Apply(); err != nil {
			return err
		}
		if mode == "build" {
			fmt.Printf("%s now serves the production build (%s/) — the dev server is stopped.\n",
				a.State.Host(*site), site.BuildDir)
		} else {
			fmt.Printf("%s now serves the dev server.\n", a.State.Host(*site))
		}
		return nil
	},
}

func a2() *app.App { return mustApp() }

// devTarget resolves which node site a dev command addresses: explicit
// name, or the site linked to the current directory.
func devTarget(a *app.App, args []string) (*app.App, *config.Site, error) {
	name, err := siteNameFromArgOrCwd(a, args)
	if err != nil {
		return nil, nil, err
	}
	site := a.State.FindSite(name)
	if site == nil {
		return nil, nil, fmt.Errorf("no site named %q", name)
	}
	if site.Kind != "node" {
		return nil, nil, fmt.Errorf("%s is not a frontend site", site.Name)
	}
	return a, site, nil
}

func init() {
	devCmd.AddCommand(devStartCmd, devStopCmd, devRestartCmd)
	rootCmd.AddCommand(devCmd, serveCmd)
}
