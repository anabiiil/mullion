package cmd

import (
	"fmt"
	"strconv"
	"time"

	"pm/internal/console"

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
the background. Stop one when you don't need it — opening its link later
wakes it automatically (a "starting…" page shows while it boots).`,
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
	Short: "Stop a site's dev server (frees RAM/CPU) — opening its link wakes it again",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a, site, err := devTarget(a2(), args)
		if err != nil {
			return err
		}
		if console.Interactive() &&
			!askYesNo(fmt.Sprintf("Stop %s's dev server?", site.Name), true) {
			fmt.Println("Kept running.")
			return nil
		}
		site.DevPaused = true
		devserver.Stop(a.Paths, site.Name)
		if err := a.Apply(); err != nil {
			return err
		}
		fmt.Printf("%s's dev server is stopped — opening %s wakes it again automatically.\n",
			site.Name, a.State.Host(*site))
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
		return a.RestartDevServer(site.Name)
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

var (
	devIdleNoTab, devIdleWithTab, devIdleSite string
	devIdleNever, devIdleSleep, devIdleReset  bool
)

// idleMinutes parses a --no-tab/--with-tab value ("5m", "1h30m", or a
// bare number of minutes) into whole minutes, 1 at least.
func idleMinutes(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		v = strconv.Itoa(n) + "m"
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (try 5m or 1h)", v)
	}
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 1 || m > config.MaxDevIdleMin {
		return 0, fmt.Errorf("%s is out of range — use 1m to 24h", v)
	}
	return m, nil
}

var devIdleCmd = &cobra.Command{
	Use:   "idle",
	Short: "Show or set how long an idle dev server runs before it sleeps",
	Long: `A frontend site's dev server sleeps after a while without requests or
source edits — sooner when no browser tab has it open — and wakes when
you open its link. Without flags this shows the limits; --site sets one
site's own (--reset makes it follow the global setting again).`,
	Example: `  mullion dev idle --no-tab 5m --with-tab 30m
  mullion dev idle --never
  mullion dev idle --site shop --never
  mullion dev idle --site shop --reset`,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := a2()
		noTab, err := idleMinutes(devIdleNoTab)
		if err != nil {
			return err
		}
		withTab, err := idleMinutes(devIdleWithTab)
		if err != nil {
			return err
		}
		changing := noTab > 0 || withTab > 0 || devIdleNever || devIdleSleep || devIdleReset
		if devIdleSite != "" {
			if changing {
				mode := "custom"
				switch {
				case devIdleReset:
					mode = "global"
				case devIdleNever:
					mode = "never"
				}
				if err := a.SetSiteDevIdle(devIdleSite, mode, noTab, withTab); err != nil {
					return err
				}
			}
			s := a.State.FindSite(devIdleSite)
			if s == nil {
				return fmt.Errorf("no site named %q", devIdleSite)
			}
			src := "follows the global setting"
			if s.DevIdleNever || s.DevIdleNoTabMin > 0 || s.DevIdleWithTabMin > 0 {
				src = "its own setting"
			}
			printIdle(s.Name+" ("+src+")", a.State.Config, *s)
			return nil
		}
		if changing {
			c := a.State.Config
			if noTab == 0 {
				noTab = c.DevIdleNoTabMin
			}
			if withTab == 0 {
				withTab = c.DevIdleWithTabMin
			}
			never := c.DevNeverSleep
			if devIdleNever {
				never = true
			}
			if devIdleSleep || devIdleReset {
				never = false
			}
			if devIdleReset {
				noTab, withTab = 0, 0
			}
			if err := a.SetDevIdle(noTab, withTab, never); err != nil {
				return err
			}
		}
		printIdle("all dev servers", a.State.Config, config.Site{})
		return nil
	},
}

func printIdle(label string, c config.Config, s config.Site) {
	noTab, withTab, never := c.DevIdle(s)
	if never {
		fmt.Printf("%s: never sleep\n", label)
		return
	}
	fmt.Printf("%s: sleep after %s idle with no tab open, %s with a tab open\n", label, noTab, withTab)
}

func init() {
	devIdleCmd.Flags().StringVar(&devIdleNoTab, "no-tab", "", "idle time before sleeping when no browser tab is open (e.g. 5m)")
	devIdleCmd.Flags().StringVar(&devIdleWithTab, "with-tab", "", "idle time before sleeping while a tab is open (e.g. 30m)")
	devIdleCmd.Flags().BoolVar(&devIdleNever, "never", false, "never put dev servers to sleep")
	devIdleCmd.Flags().BoolVar(&devIdleSleep, "sleep", false, "turn sleeping back on after --never")
	devIdleCmd.Flags().BoolVar(&devIdleReset, "reset", false, "back to the defaults (with --site: follow the global setting)")
	devIdleCmd.Flags().StringVar(&devIdleSite, "site", "", "set or show one site's own limits")
	devCmd.AddCommand(devIdleCmd)
}

func init() {
	devCmd.AddCommand(devStartCmd, devStopCmd, devRestartCmd)
	rootCmd.AddCommand(devCmd, serveCmd)
}
