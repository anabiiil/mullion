package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/caddy"
	"pm/internal/devserver"
	"pm/internal/fcgi"
	"pm/internal/mysql"
	"pm/internal/pmdir"
	"pm/internal/sysproc"
	"pm/internal/term"
	"pm/internal/version"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the whole stack — paste the output when reporting a problem",
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		ok := func(b bool) string {
			if b {
				return term.Green("ok")
			}
			return term.Red("PROBLEM")
		}

		fmt.Println("mullion", version.Number)
		if exe, err := os.Executable(); err == nil {
			fmt.Println("running from:", exe)
		}
		if found, err := exec.LookPath("mullion"); err == nil {
			fmt.Println("PATH resolves to:", found)
		}
		fmt.Println("home:", a.Paths.Home)

		// Caddy identity — the #1 cause of "my changes don't show up".
		running := caddy.Running()
		ours := running && caddy.ServingOurs(a.Paths)
		switch {
		case !running:
			fmt.Printf("caddy: %s not running\n", term.Red("PROBLEM"))
		case !ours:
			pid, name := sysproc.PortOwner(2019)
			fmt.Printf("caddy: %s a FOREIGN caddy answers the admin port (%s, PID %d) — `mullion start` will replace it\n",
				term.Red("PROBLEM"), name, pid)
		default:
			fmt.Printf("caddy: %s running with Mullion's config\n", term.Green("ok"))
		}
		for _, port := range []int{80, 443} {
			if pid, name := sysproc.PortOwner(port); pid > 0 {
				owned := strings.Contains(strings.ToLower(name), "caddy") && ours
				fmt.Printf("port %d: %s held by %s (PID %d)\n", port, ok(owned), name, pid)
			} else {
				fmt.Printf("port %d: %s nothing listening\n", port, term.Red("PROBLEM"))
			}
		}

		if v := a.State.Config.GlobalPHP; v != "" {
			runningFcgi := len(fcgi.RunningVersions([]string{v})) == 1
			fmt.Printf("php %s: %s\n", v, ok(runningFcgi))
		}
		if v := a.State.Config.MySQL; v != "" {
			foreign := mysql.Running() && len(sysproc.ProcessesUnder(a.Paths.Home, pmdir.ExeName("mysqld"))) == 0
			switch {
			case foreign:
				fmt.Printf("mysql %s: %s a foreign server holds port %d — `mullion mysql start` reclaims it\n", v, term.Red("PROBLEM"), mysql.Port)
			case mysql.Running():
				fmt.Printf("mysql %s: %s\n", v, term.Green("ok"))
			default:
				fmt.Printf("mysql %s: %s not running\n", v, term.Red("PROBLEM"))
			}
		}
		if v := a.State.Config.GlobalNode; v != "" {
			fmt.Println("node default:", v)
		}

		for _, site := range a.State.Sites {
			host := a.State.Host(site)
			switch {
			case site.Kind == "node" && site.Mode == "build":
				_, err := os.Stat(filepath.Join(site.Path, site.BuildDir, "index.html"))
				fmt.Printf("site %-24s build mode (%s/): %s\n", host, site.BuildDir, ok(err == nil))
			case site.Kind == "node":
				port := devserver.Running(a.Paths, site.Name)
				state := fmt.Sprintf("dev on port %d", port)
				if port == 0 {
					if site.DevPaused {
						state = "dev PAUSED by you"
					} else {
						state = "dev NOT RUNNING"
					}
				}
				nodeV := "?"
				if dir, err := a.NodeVersionDirFor(site); err == nil {
					nodeV = filepath.Base(dir)
				}
				fmt.Printf("site %-24s %s  (node %s): %s\n", host, state, nodeV, ok(port > 0 || site.DevPaused))
				if port == 0 && !site.DevPaused {
					if tail := devserver.LogTail(a.Paths, site.Name); tail != "" {
						fmt.Print(tail)
					}
				}
			case site.Kind == "static":
				_, err := os.Stat(filepath.Join(site.Path, site.BuildDir, "index.html"))
				fmt.Printf("site %-24s static (%s/): %s\n", host, site.BuildDir, ok(err == nil))
			default:
				fmt.Printf("site %-24s php %s\n", host, a.SiteVersion(site))
			}
		}

		// Does the Caddyfile on disk match what this state would generate?
		if data, err := os.ReadFile(a.Paths.Caddyfile()); err == nil {
			for _, site := range a.State.Sites {
				if !strings.Contains(string(data), a.State.Host(site)) {
					fmt.Printf("caddyfile: %s %s is missing — run `mullion start`\n", term.Red("PROBLEM"), a.State.Host(site))
				}
			}
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
