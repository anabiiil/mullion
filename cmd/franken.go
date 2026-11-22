package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
)

var serverWorker bool

var serverCmd = &cobra.Command{
	Use:   "server <site> [fpm|frankenphp]",
	Short: "Choose what runs a PHP site: the shared php-fpm or its own FrankenPHP server",
	Long: `Runs a PHP site on the shared php-fpm (the default) or on its own
FrankenPHP server behind Mullion's Caddy. The first switch to FrankenPHP
downloads the latest release. --worker runs Laravel Octane's FrankenPHP
worker mode (needs laravel/octane in the project).

With only a site, prints which server runs it.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		name := slugify(args[0])
		site := a.State.FindSite(name)
		if site == nil {
			return fmt.Errorf("no site named %q", name)
		}
		if len(args) == 1 {
			if serverWorker {
				return fmt.Errorf("--worker needs a server: mullion server %s frankenphp --worker", name)
			}
			return serverStatus(a, name)
		}
		progress := func(s string) { fmt.Println(s) }
		if err := a.SetSiteServer(cmd.Context(), name, args[1], serverWorker, progress); err != nil {
			return err
		}
		site = a.State.FindSite(name)
		if site.OnFranken() {
			mode := ""
			if site.Franken.Worker {
				mode = " in worker mode"
			}
			fmt.Printf("%s now runs on FrankenPHP %s%s\n", a.State.Host(*site), site.Franken.Tag, mode)
		} else {
			fmt.Printf("%s now runs on php-fpm\n", a.State.Host(*site))
		}
		return nil
	},
}

func serverStatus(a *app.App, name string) error {
	info, err := a.FrankenInfo(name)
	if err != nil {
		return err
	}
	if info.Server != "frankenphp" {
		fmt.Println("server:   php-fpm")
		if len(info.Installed) > 0 {
			fmt.Println("frankenphp installed: " + strings.Join(info.Installed, ", "))
		}
		return nil
	}
	mode := "classic"
	if info.Worker {
		mode = "worker (Laravel Octane)"
	}
	state := "stopped"
	if info.Running {
		state = "running"
	}
	fmt.Printf("server:   FrankenPHP %s\n", info.Tag)
	if info.PHPVersion != "" {
		fmt.Printf("php:      %s\n", info.PHPVersion)
	}
	fmt.Printf("mode:     %s\n", mode)
	fmt.Printf("state:    %s on 127.0.0.1:%d\n", state, info.Port)
	fmt.Printf("log:      %s\n", info.Log)
	return nil
}

var frankenCmd = &cobra.Command{
	Use:   "frankenphp",
	Short: "Manage the FrankenPHP releases sites run on",
}

var frankenUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Install the latest FrankenPHP and move every FrankenPHP site onto it",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		tag, err := a.UpdateFranken(cmd.Context(), func(s string) { fmt.Println(s) })
		if err != nil {
			return err
		}
		fmt.Printf("FrankenPHP sites now run on %s\n", tag)
		return nil
	},
}

func init() {
	serverCmd.Flags().BoolVar(&serverWorker, "worker", false, "run Laravel Octane's FrankenPHP worker mode")
	frankenCmd.AddCommand(frankenUpdateCmd)
	rootCmd.AddCommand(serverCmd, frankenCmd)
}
