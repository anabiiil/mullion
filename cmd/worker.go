package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/term"
)

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Manage a site's background workers (queue workers, the scheduler, daemons)",
	Long: `Workers are long-running commands Mullion keeps up for a site: a
Laravel queue worker (php artisan queue:work), the scheduler (php artisan
schedule:work — no cron needed), Horizon, a Node job runner...

Each runs in the site's folder with the site's PHP and Node on PATH, is
logged to ~/.mullion/logs/worker-<site>-<id>.log, and — when added with
--autostart — is started with the stack and restarted if it crashes.`,
}

var workerListCmd = &cobra.Command{
	Use:   "list [site]",
	Short: "List workers and whether they are running (all sites by default)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		var sites []config.Site
		if len(args) == 1 {
			site := a.State.FindSite(slugify(args[0]))
			if site == nil {
				return fmt.Errorf("no site named %q", args[0])
			}
			sites = append(sites, *site)
		} else {
			sites = a.State.Sites
		}
		found := false
		for _, s := range sites {
			views := a.WorkersStatus(s.Name)
			if len(views) == 0 {
				continue
			}
			found = true
			fmt.Println(term.Bold(s.Name))
			for _, v := range views {
				fmt.Printf("  %-24s %-14s %s\n", v.ID, workerStateLabel(v), v.Command)
				detail := []string{v.Kind}
				if v.AutoStart {
					detail = append(detail, "autostart")
				}
				if v.Status.Running && !v.Status.StartedAt.IsZero() {
					detail = append(detail, "up "+time.Since(v.Status.StartedAt).Round(time.Second).String())
				}
				if v.Status.Restarts > 0 {
					detail = append(detail, fmt.Sprintf("%d restarts", v.Status.Restarts))
				}
				if !v.Status.Running && v.Status.LastExit != "" {
					detail = append(detail, "last: "+v.Status.LastExit)
				}
				fmt.Println("  " + term.Dim(strings.Repeat(" ", 25)+strings.Join(detail, " · ")))
			}
		}
		if !found {
			fmt.Println("No workers yet. Add one with: mullion worker add <site> --name \"Queue\" --command \"php artisan queue:work\" --autostart")
		}
		return nil
	},
}

func workerStateLabel(v app.WorkerView) string {
	switch v.Status.State {
	case "running":
		return term.Green("running")
	case "crash-looping":
		return term.Red("crash-looping")
	case "restarting":
		return term.Yellow("restarting")
	case "paused":
		return term.Dim("paused")
	}
	return term.Dim(v.Status.State)
}

var (
	workerName      string
	workerCommand   string
	workerKind      string
	workerAutostart bool
	workerLogLines  int
)

var workerAddCmd = &cobra.Command{
	Use:   "add <site> --command <cmd>",
	Short: "Add a worker to a site",
	Example: `  mullion worker add shop --name Queue --kind queue --command "php artisan queue:work --tries=3" --autostart
  mullion worker add shop --name Scheduler --kind scheduler --command "php artisan schedule:work" --autostart`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		w, err := a.AddWorker(slugify(args[0]), config.Worker{
			Name: workerName, Command: workerCommand, Kind: workerKind, AutoStart: workerAutostart,
		})
		if err != nil {
			if w.ID != "" {
				return fmt.Errorf("added %s, but it failed to start: %w", w.ID, err)
			}
			return err
		}
		if w.AutoStart {
			fmt.Printf("Added and started worker %s (%s).\n", w.ID, w.Command)
		} else {
			fmt.Printf("Added worker %s — start it with: mullion worker start %s %s\n", w.ID, slugify(args[0]), w.ID)
		}
		return nil
	},
}

// workerAction builds start/stop/restart/remove, which share a shape.
func workerAction(use, short, done string, fn func(a *app.App, site, id string) error) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <site> <id>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a := mustApp()
			site, id := slugify(args[0]), args[1]
			if err := fn(a, site, id); err != nil {
				return err
			}
			fmt.Printf("Worker %s of %s %s.\n", id, site, done)
			return nil
		},
	}
}

var workerLogsCmd = &cobra.Command{
	Use:   "logs <site> <id>",
	Short: "Show the end of a worker's log",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site := slugify(args[0])
		if a.State.FindSite(site) == nil {
			return fmt.Errorf("no site named %q", args[0])
		}
		out := a.WorkerLog(site, args[1], workerLogLines)
		if out == "" {
			fmt.Println("(no log output yet)")
			return nil
		}
		fmt.Println(out)
		return nil
	},
}

func init() {
	workerAddCmd.Flags().StringVar(&workerName, "name", "", "label shown in lists and the panel (defaults to the command)")
	workerAddCmd.Flags().StringVar(&workerCommand, "command", "", "command to run in the site's folder (required)")
	workerAddCmd.Flags().StringVar(&workerKind, "kind", "custom", "queue, scheduler or custom")
	workerAddCmd.Flags().BoolVar(&workerAutostart, "autostart", false, "start it now, with the stack, and restart it if it crashes")
	_ = workerAddCmd.MarkFlagRequired("command")
	workerLogsCmd.Flags().IntVarP(&workerLogLines, "lines", "n", 50, "number of lines to show")

	workerCmd.AddCommand(
		workerListCmd,
		workerAddCmd,
		workerAction("start", "Start a worker (and unpause it)", "started",
			func(a *app.App, site, id string) error { return a.StartWorker(site, id) }),
		workerAction("stop", "Stop a worker (it stays stopped until started again)", "stopped",
			func(a *app.App, site, id string) error { return a.StopWorker(site, id) }),
		workerAction("restart", "Restart a worker (picks up code and .env changes)", "restarted",
			func(a *app.App, site, id string) error { return a.RestartWorker(site, id) }),
		workerAction("remove", "Stop and delete a worker", "removed",
			func(a *app.App, site, id string) error { return a.RemoveWorker(site, id) }),
		workerLogsCmd,
	)
	rootCmd.AddCommand(workerCmd)
}
