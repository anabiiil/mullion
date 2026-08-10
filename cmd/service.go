package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"pm/internal/caddy"
	"pm/internal/fcgi"
	"pm/internal/mysql"
	"pm/internal/phpver"
	"pm/internal/term"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Caddy and the php-cgi processes for all sites",
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
		if v := a.State.Config.MySQL; v != "" {
			if err := mysql.Start(a.Paths, v); err != nil {
				return err
			}
		}
		fmt.Println(term.Green("✓ Mullion is running."))
		return statusCmd.RunE(cmd, nil)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop Caddy and all php-cgi processes",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		if err := caddy.Stop(a.Paths); err != nil {
			return err
		}
		if err := fcgi.StopAll(a.Paths); err != nil {
			return err
		}
		if v := a.State.Config.MySQL; v != "" {
			if err := mysql.Stop(a.Paths, v); err != nil {
				return err
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
		if err := stopCmd.RunE(cmd, nil); err != nil {
			return err
		}
		return startCmd.RunE(cmd, nil)
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
			fmt.Printf("php-cgi:  %-10s port %d  %s\n", v, port, state)
		}

		if v := a.State.Config.MySQL; v != "" {
			state := term.Red("stopped") + " (run `mullion mysql start`)"
			if mysql.Running() {
				state = term.Green("running")
			}
			fmt.Printf("db:       %-14s port %d  %s\n", v, mysql.Port, state)
		}

		fmt.Printf("sites:    %d linked (.%s)\n", len(a.State.Sites), a.State.Config.TLD)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(startCmd, stopCmd, restartCmd, statusCmd)
}
