package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/profiler"
	"pm/internal/term"
)

var (
	profileMode     string
	profileCount    int
	profilePath     string
	profileBuiltins bool
	profileJSON     bool
	profileTop      int
	profileDepth    int
)

var profileCmd = &cobra.Command{
	Use:   "profile <site> [on|off|status|list|show <id>|clear [<id>]]",
	Short: "Profile a site's PHP requests with SPX: flame graphs and the slowest functions",
	Long: `The profiler records where a PHP request spends its time — every function
call, as a flame graph and a table of the slowest functions (the project
page's Performance → Profiler tab shows the same profiles).

It uses SPX (github.com/NoiseByNorthwest/php-spx, GPLv3). The first time,
Mullion builds it for the site's PHP version from source on this Mac
(about a minute; needs the Xcode Command Line Tools) and loads it in that
version's php.ini, disabled — it costs nothing until a site turns it on.
Turning it on passes the site's SPX settings through Caddy, for that site
only. macOS only for now: SPX has no Windows build.

What gets profiled (--mode):
  browser   only requests you ask for (default): SPX's control panel on
            the site switches it on for your browser session, or add
            ?SPX_ENABLED=1&SPX_KEY=<key> to any URL of the site
  next      the next --count requests (default 10; --path to filter)
  path      every request whose path matches --path (/api, /orders/*)
  all       every request (each one writes a report)

  mullion profile blog on                       # profile when asked
  mullion profile blog on --mode next --count 5 --path /api
  mullion profile blog list
  mullion profile blog show latest             # top functions + hot path
  mullion profile blog clear
  mullion profile blog off

Profiles live in ~/.mullion/profiler/<site>/reports; the newest 100 (and at
most 256 MB) of each site are kept.`,
	Args: cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		name := slugify(args[0])
		action := "status"
		if len(args) >= 2 {
			action = args[1]
		}
		if len(args) == 3 && action != "show" && action != "clear" {
			return fmt.Errorf("unexpected argument %q", args[2])
		}
		switch action {
		case "status":
			info, err := a.ProfilerStatus(name)
			if err != nil {
				return err
			}
			return printProfilerStatus(info)
		case "on":
			s := profiler.Site{Mode: profiler.Mode(profileMode), Count: profileCount, Path: profilePath, Builtins: profileBuiltins}
			if _, err := profiler.ParseMode(profileMode); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			info, err := a.SetProfiler(ctx, name, true, s, func(line string) { fmt.Println(line) })
			if err != nil {
				return err
			}
			fmt.Println(term.Green("✓"), "Profiler on for", info.Site)
			return printProfilerStatus(info)
		case "off":
			info, err := a.SetProfiler(context.Background(), name, false, profiler.Site{}, nil)
			if err != nil {
				return err
			}
			fmt.Println(term.Green("✓"), "Profiler off for", info.Site, "(profiles kept — `mullion profile", info.Site, "clear` deletes them)")
			return nil
		case "list":
			reports, err := a.ProfilerReports(name)
			if err != nil {
				return err
			}
			if profileJSON {
				return printProfileJSON(reports)
			}
			if len(reports) == 0 {
				fmt.Println("No profiles yet.")
				return nil
			}
			for _, r := range reports {
				req := r.Method + " " + r.URI
				if r.CLI {
					req = "CLI " + r.Command
				}
				fmt.Printf("%s  %9s  %8s  %7d calls  %s\n",
					time.Unix(r.Time, 0).Format("2006-01-02 15:04:05"),
					fmtProfileNs(float64(r.WallUs)*1000), fmtProfileBytes(float64(r.PeakMemory)), r.Calls, req)
				fmt.Printf("  %s\n", term.Dim(r.ID))
			}
			return nil
		case "show":
			if len(args) < 3 {
				return fmt.Errorf("say which profile: mullion profile %s show <id|latest>", name)
			}
			return showProfile(a, name, args[2])
		case "clear":
			id := ""
			if len(args) == 3 {
				id = args[2]
			}
			n, err := a.DeleteProfilerReport(name, id)
			if err != nil {
				return err
			}
			fmt.Println(term.Green("✓"), "Deleted", n, "profile(s)")
			return nil
		}
		return fmt.Errorf("unknown action %q (on, off, status, list, show, clear)", action)
	},
}

func printProfileJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printProfilerStatus(info app.ProfilerInfo) error {
	if profileJSON {
		return printProfileJSON(info)
	}
	if !info.Supported {
		fmt.Println("Profiler: not available —", info.Note)
		return nil
	}
	built := "built"
	if !info.SpxLoaded {
		built = "not built yet (`mullion profile " + info.Site + " on` builds it)"
	}
	fmt.Printf("Site:      %s (PHP %s)\nSPX:       %s, %s\n", info.Site, info.PHP, info.SpxVersion, built)
	if !info.Enabled {
		fmt.Println("Profiling: off")
	} else {
		s := info.Settings
		mode := string(s.Mode)
		switch s.Mode {
		case profiler.ModeNext:
			mode = fmt.Sprintf("next %d requests", s.Count)
			if s.Path != "" {
				mode += " matching " + s.Path
			}
		case profiler.ModePath:
			mode = "requests matching " + s.Path
		case profiler.ModeAll:
			mode = "every request"
		default:
			mode = "when asked (browser session or ?SPX_ENABLED=1)"
		}
		if info.Gate != nil {
			mode += fmt.Sprintf(" — %d captured", info.Gate.Captured)
		}
		fmt.Printf("Profiling: on, %s\n", mode)
		if info.Finished {
			fmt.Println("           (all requested profiles captured; back to “when asked”)")
		}
		fmt.Printf("Control:   %s\nOne-off:   %s\n", info.ControlURL, info.ProfileURL)
		if !info.Wired {
			fmt.Println(term.Yellow("Note:"), "the Caddyfile doesn't pass this site's settings to PHP yet — run `mullion start`.")
		}
	}
	fmt.Printf("Profiles:  %d (%s) in %s\n", info.Reports, fmtProfileBytes(float64(info.Bytes)), info.Dir)
	return nil
}

func showProfile(a *app.App, site, id string) error {
	if id == "latest" {
		reports, err := a.ProfilerReports(site)
		if err != nil {
			return err
		}
		if len(reports) == 0 {
			return fmt.Errorf("%s has no profiles yet", site)
		}
		id = reports[0].ID
	}
	m, p, err := a.ProfilerReport(site, id, 0.0002)
	if err != nil {
		return err
	}
	if profileJSON {
		return printProfileJSON(map[string]any{"meta": m, "profile": p})
	}
	req := m.Method + " " + m.URI
	if m.CLI {
		req = "CLI " + m.Command
	}
	fmt.Printf("%s  %s\n", term.Bold(req), term.Dim(time.Unix(m.Time, 0).Format("2006-01-02 15:04:05")))
	fmt.Printf("wall %s · peak memory %s · %d calls of %d functions\n\n",
		fmtProfileNs(float64(m.WallUs)*1000), fmtProfileBytes(float64(m.PeakMemory)), m.Calls, m.Functions)

	short := func(name string) string { return name }
	if s := a.State.FindSite(site); s != nil && s.Path != "" {
		prefix := strings.TrimRight(filepath.ToSlash(s.Path), "/") + "/"
		short = func(name string) string { return strings.ReplaceAll(name, prefix, "") }
	}
	total := p.Root.Total
	fmt.Println(term.Bold("Top functions by self time"))
	for i, f := range p.Funcs {
		if i >= profileTop {
			break
		}
		fmt.Printf("  %9s %5.1f%%  incl %9s  %7d×  %s\n", fmtProfileNs(f.Self), f.Self/nonZero(total)*100, fmtProfileNs(f.Inclusive), f.Calls, short(f.Name))
	}
	fmt.Println()
	fmt.Println(term.Bold("Call tree (frames over 2% of the time)"))
	var walk func(n *profiler.Node, depth int)
	walk = func(n *profiler.Node, depth int) {
		if depth > profileDepth {
			return
		}
		for _, c := range n.Children {
			if c.Total < total*0.02 {
				continue
			}
			fmt.Printf("  %s%5.1f%%  %s  %s\n", strings.Repeat("  ", depth), c.Total/nonZero(total)*100, fmtProfileNs(c.Total), short(c.Name))
			walk(c, depth+1)
		}
	}
	walk(p.Root, 0)
	return nil
}

func nonZero(f float64) float64 {
	if f == 0 {
		return 1
	}
	return f
}

func fmtProfileNs(ns float64) string {
	switch {
	case ns >= 1e9:
		return fmt.Sprintf("%.2f s", ns/1e9)
	case ns >= 1e6:
		return fmt.Sprintf("%.2f ms", ns/1e6)
	case ns >= 1e3:
		return fmt.Sprintf("%.1f µs", ns/1e3)
	}
	return fmt.Sprintf("%.0f ns", ns)
}

func fmtProfileBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", b/(1<<10))
	}
	return fmt.Sprintf("%.0f B", b)
}

func init() {
	profileCmd.Flags().StringVar(&profileMode, "mode", "browser", "on: what to profile — browser, next, path or all")
	profileCmd.Flags().IntVar(&profileCount, "count", 10, "on --mode next: how many requests")
	profileCmd.Flags().StringVar(&profilePath, "path", "", "on --mode next|path: only request paths matching this prefix or glob (/api, /orders/*)")
	profileCmd.Flags().BoolVar(&profileBuiltins, "builtins", false, "on: also profile PHP's internal functions")
	profileCmd.Flags().BoolVar(&profileJSON, "json", false, "status/list/show: print JSON")
	profileCmd.Flags().IntVar(&profileTop, "top", 20, "show: how many functions to list")
	profileCmd.Flags().IntVar(&profileDepth, "depth", 12, "show: call-tree depth")
	rootCmd.AddCommand(profileCmd)
}
