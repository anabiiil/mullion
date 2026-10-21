package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/config"
	"pm/internal/term"
	"pm/internal/watch"
)

var (
	watchBasic, watchNoDumps, watchNoDeep bool
	watchTypes                            string
	watchJSON                             bool
	watchBacklog                          int
	watchClearLogs                        bool
)

var watchCmd = &cobra.Command{
	Use:   "watch <site> [on|off|status|tail|clear]",
	Short: "Watch a site live: requests, logs, dumps, queries, jobs… (the project page's Watch tab)",
	Long: `The live watcher shows what a site does while you use it. Everything stays
on this computer; it is off until you turn it on.

  Requests & logs   Caddy's access log and storage/logs/*.log — nothing in
                    the project changes.
  Dumps             dump()/dd() go to Mullion instead of the page.
  Deep (Laravel)    queries (N+1, duplicates, slow), jobs, cache, events,
                    views, HTTP client calls and a per-request timeline.

Dumps and Deep load a small PHP file before the site's code
(auto_prepend_file, passed by Caddy as PHP_VALUE for this site only — no
php.ini or Composer change). On Windows, where php-cgi ignores
PHP_VALUE, php-cgi loads it through an extra ini scan dir while any site
is watched; it does nothing for sites that aren't. Queue workers and
artisan commands run by Mullion get it through PHP_INI_SCAN_DIR; running
workers (and php-cgi on Windows) restart when it is turned on or off.

  mullion watch blog on            # requests, logs, dumps and deep
  mullion watch blog on --basic    # requests & logs only
  mullion watch blog tail --type queries,exceptions
  mullion watch blog clear          # forget what was recorded so far
  mullion watch blog clear --logs   # …and empty storage/logs/*.log too`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site := a.State.FindSite(slugify(args[0]))
		if site == nil {
			return fmt.Errorf("no site named %q", args[0])
		}
		action := "status"
		if len(args) == 2 {
			action = args[1]
		}
		switch action {
		case "status":
			return watchStatus(a, site.Name)
		case "on":
			w := config.Watch{}
			if site.Watch != nil {
				w = *site.Watch
			}
			w.Enabled = true
			w.Dumps = site.IsPHP() && !watchBasic && !watchNoDumps
			w.Deep = site.IsPHP() && !watchBasic && !watchNoDeep
			if err := a.SetWatch(site.Name, w); err != nil {
				return err
			}
			fmt.Println(term.Green("✓ Watching " + site.Name))
			return watchStatus(a, site.Name)
		case "off":
			w := config.Watch{}
			if site.Watch != nil {
				w = *site.Watch
			}
			w.Enabled, w.Dumps, w.Deep = false, false, false
			if err := a.SetWatch(site.Name, w); err != nil {
				return err
			}
			fmt.Println("Stopped watching " + site.Name + ".")
			return nil
		case "clear":
			emptied, err := a.ClearWatch(site.Name, watchClearLogs)
			for _, f := range emptied {
				fmt.Println("Emptied " + f)
			}
			if err != nil {
				return err
			}
			fmt.Println("Cleared — the Watch tab and `watch tail` start fresh from here.")
			return nil
		case "tail":
			return watchTail(a, site.Name)
		}
		return fmt.Errorf("unknown action %q — use on, off, status, tail or clear", action)
	},
}

func watchStatus(a *app.App, site string) error {
	info, err := a.WatchStatus(site)
	if err != nil {
		return err
	}
	st := info.Settings
	onOff := func(b bool) string {
		if b {
			return term.Green("on")
		}
		return "off"
	}
	fmt.Printf("Watch %s: %s\n", site, onOff(st.Enabled))
	if !st.Enabled {
		fmt.Println("  Turn it on with: mullion watch " + site + " on")
		return nil
	}
	fmt.Printf("  Requests & logs  on\n  Dumps            %s%s\n  Deep             %s\n", onOff(st.Dumps),
		map[bool]string{true: " (also shown in the page)", false: ""}[st.Dumps && st.DumpsInPage], onOff(st.Deep))
	fmt.Printf("  Thresholds       slow request %d ms · slow query %d ms · N+1 after %d\n", st.SlowRequestMs, st.SlowQueryMs, st.NPlusOne)
	fmt.Printf("  Events           %s\n", info.Dir)
	if info.Note != "" {
		fmt.Println("  " + info.Note)
	}
	if st.Deep && !info.Laravel {
		fmt.Println("  The deep watcher hooks into Laravel — this doesn't look like a Laravel app.")
	}
	return nil
}

// watchTail prints the site's watcher items as they arrive (Ctrl+C stops).
func watchTail(a *app.App, site string) error {
	feed, err := a.WatchFeed(site)
	if err != nil {
		return err
	}
	types, err := watchTypeSet(watchTypes)
	if err != nil {
		return err
	}
	want := func(it watch.Item) bool { return len(types) == 0 || types[it.Type()] }
	st := feed.Settings()
	print := func(items []watch.Item) {
		for _, it := range items {
			if !want(it) {
				continue
			}
			if watchJSON {
				b, _ := json.Marshal(it)
				fmt.Println(string(b))
				continue
			}
			fmt.Println(formatWatchItem(it, st))
		}
	}
	print(feed.Backlog(watchBacklog))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			return nil
		case <-tick.C:
			print(feed.Poll())
		}
	}
}

// watchTypeSet parses --type: plural or singular names; "queries" also
// shows the per-request analysis (N+1, duplicates).
func watchTypeSet(list string) (map[string]bool, error) {
	alias := map[string][]string{
		"request": {"request"}, "requests": {"request"}, "query": {"query", "analysis"}, "queries": {"query", "analysis"},
		"log": {"log"}, "logs": {"log", "exception"}, "exception": {"exception"}, "exceptions": {"exception"},
		"job": {"job"}, "jobs": {"job"}, "dump": {"dump"}, "dumps": {"dump"}, "cache": {"cache"},
		"event": {"event"}, "events": {"event"}, "view": {"view"}, "views": {"view"}, "http": {"http"},
		"mail": {"mail"}, "commands": {"command"}, "command": {"command"},
	}
	out := map[string]bool{}
	for _, t := range strings.Split(list, ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			continue
		}
		names, ok := alias[t]
		if !ok {
			return nil, fmt.Errorf("unknown type %q", t)
		}
		for _, n := range names {
			out[n] = true
		}
	}
	return out, nil
}

// num reads a JSON number (float64) or one the Go side set (int, int64).
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
func str(v any) string { s, _ := v.(string); return s }

func formatWatchItem(it watch.Item, st watch.Settings) string {
	ts := time.UnixMilli(int64(it.Time() * 1000)).Format("15:04:05.000")
	head := term.Dim(ts) + " " + fmt.Sprintf("%-9s", it.Type())
	loc := func() string {
		if f := str(it["file"]); f != "" {
			return term.Dim(fmt.Sprintf("  %s:%d", f, int(num(it["line"]))))
		}
		return ""
	}
	switch it.Type() {
	case "request":
		status := it["status"]
		ms := num(it["ms"])
		line := fmt.Sprintf("%s %v %s %.0f ms", str(it["method"]), status, str(it["uri"]), ms)
		if ms >= float64(st.SlowRequestMs) {
			line = term.Yellow(line + " (slow)")
		} else if num(status) >= 500 {
			line = term.Red(line)
		}
		return head + line
	case "query":
		line := fmt.Sprintf("%.2f ms  %s", num(it["ms"]), str(it["sql"]))
		if num(it["ms"]) >= float64(st.SlowQueryMs) {
			line = term.Yellow(line)
		}
		return head + line + loc()
	case "analysis":
		out := head + fmt.Sprintf("%v queries, %.1f ms", it["queries"], num(it["ms"]))
		if issues, ok := it["issues"].([]watch.QueryIssue); ok {
			for _, is := range issues {
				out += "\n" + strings.Repeat(" ", 23) + term.Red(fmt.Sprintf("%s ×%d", is.Kind, is.Count)) + "  " + is.SQL
				if is.File != "" {
					out += term.Dim(fmt.Sprintf("  %s:%d", is.File, is.Line))
				}
			}
		}
		return out
	case "log":
		return head + strings.ToUpper(str(it["level"])) + " " + strings.SplitN(str(it["message"]), "\n", 2)[0]
	case "exception":
		return head + term.Red(str(it["class"])) + " " + strings.SplitN(str(it["message"]), "\n", 2)[0] + loc()
	case "dump":
		return head + loc() + "\n" + str(it["text"])
	case "job":
		line := str(it["op"]) + " " + str(it["name"])
		if e := str(it["error"]); e != "" {
			line += " — " + term.Red(e)
		}
		return head + line
	case "cache":
		return head + str(it["op"]) + " " + str(it["key"])
	case "event", "view":
		return head + str(it["name"]) + loc()
	case "http":
		return head + fmt.Sprintf("%s %v %s %.0f ms", str(it["method"]), it["status"], str(it["url"]), num(it["ms"]))
	case "command":
		return head + "artisan " + str(it["command"])
	case "mail":
		return head + str(it["subject"])
	}
	return head
}

func init() {
	watchCmd.Flags().BoolVar(&watchBasic, "basic", false, "on: requests & logs only (no PHP prepend)")
	watchCmd.Flags().BoolVar(&watchNoDumps, "no-dumps", false, "on: leave dump() in the page")
	watchCmd.Flags().BoolVar(&watchNoDeep, "no-deep", false, "on: skip the deep watcher (queries, jobs, cache…)")
	watchCmd.Flags().StringVar(&watchTypes, "type", "", "tail: only these types, comma-separated (requests, queries, logs, exceptions, jobs, dumps, cache, events, views, http, mail)")
	watchCmd.Flags().BoolVar(&watchJSON, "json", false, "tail: print raw JSON lines")
	watchCmd.Flags().IntVar(&watchBacklog, "backlog", 20, "tail: recent items to show first (per source)")
	watchCmd.Flags().BoolVar(&watchClearLogs, "logs", false, "clear: also empty the project's storage/logs/*.log files")
	rootCmd.AddCommand(watchCmd)
}
