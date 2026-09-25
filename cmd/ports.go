package cmd

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/term"
)

var portsCmd = &cobra.Command{
	Use:   "ports",
	Short: "List the ports Mullion uses, who holds them, and any conflicts",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		conflicts := 0
		fmt.Printf("%-32s %-6s %-9s %s\n", "SERVICE", "PORT", "STATE", "HELD BY")
		for _, p := range a.PortsOverview() {
			state := "stopped"
			if p.Running {
				state = "running"
			}
			held := p.HeldBy
			if p.Conflict {
				conflicts++
				if held == "" {
					held = "wanted by two services"
				}
				held = term.Red("conflict: " + held)
			}
			service := p.Service
			if p.Configurable {
				service += " *"
			}
			fmt.Printf("%-32s %-6d %-9s %s\n", service, p.Port, state, held)
			if p.Note != "" {
				fmt.Println(term.Dim("    " + p.Note))
			}
		}
		fmt.Println(term.Dim("\n* configurable: mullion port set <mysql|postgres|mongo> <port>, mullion port dev <site> <port>"))
		if conflicts > 0 {
			fmt.Printf("%d conflict(s) — move the service to a free port (e.g. %d) or stop the other process.\n",
				conflicts, a.SuggestFreePort(42001))
		}
		return nil
	},
}

var portForce bool

var portCmd = &cobra.Command{
	Use:   "port",
	Short: "Change the port of a database engine or a dev server",
}

var portSetCmd = &cobra.Command{
	Use:   "set <mysql|postgres|mongo> <port>",
	Short: "Move a database engine to another port (restarts it if running)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid port %q", args[1])
		}
		a := mustApp()
		hints, err := a.SetEnginePort(args[0], port, portForce)
		if err != nil {
			return portErr(err)
		}
		fmt.Println(term.Green(fmt.Sprintf("✓ %s now uses port %d.", args[0], port)))
		printHints(hints)
		return nil
	},
}

var portDevCmd = &cobra.Command{
	Use:   "dev <site> <port>",
	Short: "Assign a frontend site's dev server another port (restarts it if running)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid port %q", args[1])
		}
		a := mustApp()
		hints, err := a.SetSiteDevPort(args[0], port, portForce)
		if err != nil {
			return portErr(err)
		}
		fmt.Println(term.Green(fmt.Sprintf("✓ %s's dev server is assigned port %d.", args[0], port)))
		printHints(hints)
		return nil
	},
}

// portErr adds the way out to a "port held by another process" error.
func portErr(err error) error {
	var inUse *app.PortInUseError
	if errors.As(err, &inUse) {
		return fmt.Errorf("%w\n(stop that process, pick a free port, or pass --force to save it anyway)", err)
	}
	return err
}

func printHints(hints []string) {
	for _, h := range hints {
		fmt.Println(term.Yellow("note: ") + h)
	}
}

func init() {
	for _, c := range []*cobra.Command{portSetCmd, portDevCmd} {
		c.Flags().BoolVar(&portForce, "force", false, "use the port even though another process holds it")
	}
	portCmd.AddCommand(portSetCmd, portDevCmd)
	rootCmd.AddCommand(portsCmd, portCmd)
}
