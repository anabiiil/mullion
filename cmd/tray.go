package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/caddy"
	"pm/internal/console"
	"pm/internal/fcgi"
	"pm/internal/mysql"
	"pm/internal/proc"
	"pm/internal/tray"
)

var trayCmd = &cobra.Command{
	Use:   "tray",
	Short: "Show the Mullion icon in the Windows notification area",
	Long: `Starts the services, hides this console, and puts a Mullion icon next
to the clock: double-click opens the dashboard, right-click gives Start,
Stop, phpMyAdmin, and Quit. This is what runs at Windows sign-in when
autostart is enabled.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		console.HideWindow()

		// Bring the stack up in the background while the icon appears.
		go startServices()

		self, _ := os.Executable()
		err := tray.Run(tray.Handlers{
			OpenDashboard: func() { spawnSelf(self, "ui") },
			StartServices: startServices,
			StopServices:  stopServices,
			OpenPMA: func() {
				a, err := app.New()
				host := "phpmyadmin.test"
				if err == nil {
					host = "phpmyadmin." + a.State.Config.TLD
				}
				_ = exec.Command("cmd", "/c", "start", "", "https://"+host).Start()
			},
		})
		if err == tray.ErrAlreadyRunning {
			return nil
		}
		return err
	},
}

func startServices() {
	a, err := app.New()
	if err != nil || a.State.Config.GlobalPHP == "" {
		return
	}
	_ = caddy.EnsureInstalled(context.Background(), a.Paths)
	_ = a.Apply()
	_ = caddy.Start(a.Paths)
	if v := a.State.Config.MySQL; v != "" {
		_ = mysql.Start(a.Paths, v)
	}
}

func stopServices() {
	a, err := app.New()
	if err != nil {
		return
	}
	_ = caddy.Stop(a.Paths)
	_ = fcgi.StopAll(a.Paths)
	if v := a.State.Config.MySQL; v != "" {
		_ = mysql.Stop(a.Paths, v)
	}
}

// spawnSelf runs another mullion command as its own hidden-console
// process, so the tray's message loop never blocks.
func spawnSelf(exe string, args ...string) {
	cmd := exec.Command(exe, args...)
	proc.DetachHiddenConsole(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "tray:", err)
		return
	}
	_ = cmd.Process.Release()
}

func init() {
	rootCmd.AddCommand(trayCmd)
}
