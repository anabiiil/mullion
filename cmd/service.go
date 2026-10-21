package cmd

import (
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"pm/internal/agent"
	"pm/internal/caddy"
	"pm/internal/devserver"
	"pm/internal/fcgi"
	"pm/internal/mailpit"
	"pm/internal/mongodb"
	"pm/internal/mssql"
	"pm/internal/mysql"
	"pm/internal/phpver"
	"pm/internal/pmdir"
	"pm/internal/postgres"
	"pm/internal/redis"
	"pm/internal/term"
)

// fcgiLabel names the per-version FastCGI worker in status output.
var fcgiLabel = map[string]string{"windows": "php-cgi"}[runtime.GOOS]

func init() {
	if fcgiLabel == "" {
		fcgiLabel = "php-fpm"
	}
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Caddy and the PHP FastCGI workers for all sites",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := caddy.EnsureInstalled(cmd.Context(), a.Paths); err != nil {
			return err
		}
		if err := a.Apply(); err != nil {
			return err
		}
		if err := caddy.Start(a.Paths); err != nil {
			return err
		}
		if v := a.State.Config.GlobalNode; v != "" {
			fmt.Printf("node:     %s (default)\n", v)
		}
		for _, site := range a.State.Sites {
			if site.Kind != "node" {
				continue
			}
			switch {
			case site.Mode == "build":
				fmt.Printf("dev:      %-14s serving the production build (%s/)\n", a.State.Host(site), site.BuildDir)
			case devserver.Running(a.Paths, site.Name) > 0:
				nodeLabel := ""
				if dir, err := a.NodeVersionDirFor(site); err == nil {
					nodeLabel = "  node " + filepath.Base(dir)
				}
				fmt.Printf("dev:      %-14s port %d  %s%s\n", a.State.Host(site),
					devserver.Running(a.Paths, site.Name), term.Green("running"), nodeLabel)
			case site.DevPaused:
				fmt.Printf("dev:      %-14s %s (start with `mullion dev start %s`)\n", a.State.Host(site), term.Yellow("paused"), site.Name)
			default:
				fmt.Printf("dev:      %-14s %s (starts with `mullion start`)\n", a.State.Host(site), term.Red("stopped"))
			}
		}

		if v := a.State.Config.MySQL; v != "" {
			reclaimMysqlPort(a, false)
			if err := mysql.Start(a.Paths, v); err != nil {
				return err
			}
			restorePendingDump(a)
		}
		if a.PhpShadow() != "" {
			reassertPathPriority(a)
		}
		fmt.Println(term.Green("✓ Mullion is running."))
		return statusCmd.RunE(cmd, nil)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop Caddy and all PHP FastCGI workers",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := caddy.Stop(a.Paths); err != nil {
			return err
		}
		if err := fcgi.StopAll(a.Paths); err != nil {
			return err
		}
		devserver.StopAll(a.Paths)
		agent.Stop(a.Paths)
		// A whole-stack stop, same as MySQL above: this does NOT set the
		// user's stopped flags, so a plain `mullion start` afterwards
		// brings every configured engine back.
		if v := a.State.Config.MySQL; v != "" {
			if err := mysql.Stop(a.Paths, v); err != nil {
				return err
			}
		}
		if v := a.State.Config.Postgres; v != "" {
			if err := postgres.Stop(a.Paths, v); err != nil {
				return err
			}
		}
		if v := a.State.Config.Mongo; v != "" {
			if err := mongodb.Stop(a.Paths, v); err != nil {
				return err
			}
		}
		if a.State.Config.MSSQL != "" {
			// Bounded: a slow container runtime must not hang `stop`.
			if err := mssql.StopFast(a.Paths); err != nil {
				fmt.Println("note: SQL Server did not stop cleanly -", err)
			}
		}
		if a.State.Config.Mailpit != "" {
			if err := mailpit.Stop(a.Paths); err != nil {
				fmt.Println("note: Mail did not stop cleanly -", err)
			}
		}
		if a.State.Config.Redis != "" {
			if err := redis.Stop(a.Paths); err != nil {
				fmt.Println("note: Redis did not stop cleanly -", err)
			}
		}
		fmt.Println("Stopped.")
		return nil
	},
}

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart everything",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := a.RestartStack(cmd.Context()); err != nil {
			return err
		}
		fmt.Println(term.Green("✓ Mullion is running."))
		return statusCmd.RunE(cmd, nil)
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show what's running",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()

		if caddy.Running() {
			fmt.Printf("caddy:    %s\n", term.Green("running"))
		} else {
			fmt.Printf("caddy:    %s (run `mullion start`)\n", term.Red("stopped"))
		}

		global := a.State.Config.GlobalPHP
		if global == "" {
			fmt.Println("php:      no global version set (run `mullion use <version>`)")
		} else {
			fmt.Printf("php:      %s (global)\n", global)
		}
		if shadow := a.PhpShadow(); shadow != "" {
			fmt.Printf("%s  `php` resolves to %s\n          (another install shadows Mullion on your PATH — see `mullion use`)\n",
				term.Yellow("warning:"), shadow)
		}
		for _, v := range a.NeededVersions() {
			port, _ := phpver.FcgiPort(v)
			state := term.Red("stopped")
			if len(fcgi.RunningVersions([]string{v})) == 1 {
				state = term.Green("running")
			}
			fmt.Printf("%-9s %-10s port %d  %s\n", fcgiLabel+":", v, port, state)
		}

		if v := a.State.Config.GlobalNode; v != "" {
			fmt.Printf("node:     %s (default)\n", v)
		}
		for _, site := range a.State.Sites {
			if site.Kind != "node" {
				continue
			}
			switch {
			case site.Mode == "build":
				fmt.Printf("dev:      %-24s serving the production build (%s/)\n", a.State.Host(site), site.BuildDir)
			case devserver.Running(a.Paths, site.Name) > 0:
				nodeLabel := ""
				if dir, err := a.NodeVersionDirFor(site); err == nil {
					nodeLabel = "  node " + filepath.Base(dir)
				}
				fmt.Printf("dev:      %-24s port %d  %s%s\n", a.State.Host(site),
					devserver.Running(a.Paths, site.Name), term.Green("running"), nodeLabel)
			default:
				fmt.Printf("dev:      %-24s %s (wakes when you open the link)\n",
					a.State.Host(site), term.Yellow("sleeping"))
			}
		}

		if v := a.State.Config.MySQL; v != "" {
			state := term.Red("stopped") + " (run `mullion mysql start`)"
			if mysql.Running() {
				state = term.Green("running")
			}
			fmt.Printf("db:       %-14s port %d  %s\n", v, mysql.Port, state)
		}

		if v := a.State.Config.Postgres; v != "" {
			state := term.Red("stopped") + " (run `mullion postgres start`)"
			if postgres.Running() {
				state = term.Green("running")
			}
			pw := "no password set"
			if a.State.Config.PostgresPassword != "" {
				pw = "password set"
			}
			fmt.Printf("db:       %-14s port %d  %s  (%s)\n", v, postgres.Port, state, pw)
		}

		if v := a.State.Config.Mongo; v != "" {
			state := term.Red("stopped") + " (run `mullion mongo start`)"
			if mongodb.Running() {
				state = term.Green("running")
			}
			fmt.Printf("db:       %-14s port %d  %s\n", v, mongodb.Port, state)
		}

		if v := a.State.Config.MSSQL; v != "" {
			running := mssql.Running()
			fmt.Println("db:       " + mssqlStatusLine(v, mssql.Port, running, a.State.Config.MSSQLStopped, runtime.GOOS, mssqlRuntime(a.Paths, running)))
		}

		if v := a.State.Config.Mailpit; v != "" {
			fmt.Println("mail:     " + mailStatusLine(v, mailpit.Owned(a.Paths) && mailpit.Running(), a.State.Config.MailStopped, a.MailURL()))
		}

		if v := a.State.Config.Redis; v != "" {
			fmt.Println("cache:    " + redisStatusLine(v, redis.Owned(a.Paths) && redis.Running(), a.State.Config.RedisStopped))
		}

		fmt.Printf("sites:    %d linked (.%s)\n", len(a.State.Sites), a.State.Config.TLD)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(startCmd, stopCmd, restartCmd, statusCmd)
}

// mssqlRuntime is the runtime check `mullion status` shows when SQL
// Server isn't answering (cheap: no docker commands).
func mssqlRuntime(paths pmdir.Paths, running bool) mssql.RuntimeStatus {
	if running {
		return mssql.RuntimeStatus{Daemon: true, Service: true}
	}
	return mssql.QuickStatus(paths)
}

// mssqlStatusLine renders SQL Server's `mullion status` line: state and
// port, plus on macOS whether OrbStack (its container runtime) is up.
func mssqlStatusLine(series string, port int, running, stoppedByUser bool, goos string, rt mssql.RuntimeStatus) string {
	label := "mssql " + series
	if running {
		return fmt.Sprintf("%-14s port %d  %s", label, port, term.Green("running"))
	}
	state := term.Red("stopped") + " (run `mullion mssql start`)"
	if stoppedByUser {
		state = term.Red("stopped") + " by you (run `mullion mssql start`)"
	}
	if goos == "darwin" {
		switch {
		case !rt.OrbStack && !rt.Docker:
			state += "  — OrbStack is not installed (`mullion mssql orbstack install`)"
		case !rt.Daemon:
			state += "  — OrbStack is not running"
		default:
			state += "  — OrbStack is up"
		}
	}
	return fmt.Sprintf("%-14s port %d  %s", label, port, state)
}

// mailStatusLine renders Mail's `mullion status` line.
func mailStatusLine(version string, running, stoppedByUser bool, url string) string {
	state := term.Red("stopped") + " (run `mullion mail start`)"
	switch {
	case running:
		state = term.Green("running") + "  " + url
	case stoppedByUser:
		state = term.Red("stopped") + " by you (run `mullion mail start`)"
	}
	return fmt.Sprintf("%-14s smtp %d  inbox %d  %s", "mailpit "+version, mailpit.SMTPPort, mailpit.UIPort, state)
}
