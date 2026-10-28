package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/loadtest"
	"pm/internal/perf"
	"pm/internal/term"
	"pm/internal/tty"
	"pm/internal/workers"
)

var (
	perfJSON        bool
	perfConc        int
	perfDuration    time.Duration
	perfRequests    int
	perfWarmUp      time.Duration
	perfMethod      string
	perfHeaders     []string
	perfOptimize    bool
	perfBaseline    string
	perfSaveBase    string
	perfShowIgnored bool
)

var perfCmd = &cobra.Command{
	Use:   "perf <site> [routes|check|load <path>|prod on|off|status|workers]",
	Short: "Performance: route timings, a production-readiness check, load tests, production-like mode, worker health",
	Long: `The project page's Performance tab, from the terminal.

  routes          per-route timings from the Watch tab's data (count,
                  p50/p95/p99, queries, peak memory, errors, N+1) and the
                  site's budgets. Needs "mullion watch <site> on"; Deep for
                  queries and memory. --save <name> keeps a baseline,
                  --compare <name> shows what changed since.
  check           the production-readiness audit: APP_DEBUG/APP_ENV and
                  friends, cache readiness (env() outside config/, route
                  names, closures), dev packages in require, OPcache, PHP
                  end of life, database indexes and primary keys, the
                  frontend build, and the N+1/slow queries Watch saw.
  load <path>     a load test against the site's own host (never anything
                  else): -c concurrency (1–200), -d duration (5s–2m) or -n
                  requests, --warmup. It runs against your local database.
  prod on|off     production-like mode: APP_ENV=production and
                  APP_DEBUG=false passed to PHP by Caddy (the .env stays as
                  it is), OPcache stops checking file timestamps for the
                  site (macOS/Linux), --optimize also runs "php artisan
                  optimize" (undone when you turn it off). Frontend sites
                  serve their build with compression and long cache
                  headers. Code changes don't show until you turn it off.
  workers         memory/CPU of the site's workers, leak detection, and
                  per-job timings from queue workers' logs.

  mullion perf blog routes
  mullion perf blog check
  mullion perf blog load /posts -c 20 -d 30s
  mullion perf blog prod on --optimize`,
	Args: cobra.RangeArgs(1, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		a := mustApp()
		site := a.State.FindSite(slugify(args[0]))
		if site == nil {
			return fmt.Errorf("no site named %q", args[0])
		}
		action := "routes"
		if len(args) >= 2 {
			action = args[1]
		}
		arg := ""
		if len(args) == 3 {
			arg = args[2]
		}
		switch action {
		case "routes":
			return perfRoutes(a, site.Name)
		case "check":
			return perfCheck(a, site.Name)
		case "load":
			return perfLoad(a, site.Name, arg)
		case "prod":
			switch arg {
			case "on", "off":
				err := a.SetProdMode(context.Background(), site.Name, arg == "on", perfOptimize, func(b []byte) { os.Stdout.Write(b) })
				if err != nil {
					return err
				}
				if arg == "on" {
					fmt.Println(term.Green("✓ "+site.Name+" runs like production") + " — code changes won't show until you run `mullion perf " + site.Name + " prod off`.")
				} else {
					fmt.Println("Production-like mode is off for " + site.Name + ".")
				}
				return nil
			case "", "status":
				st, err := a.ProdModeStatus(site.Name)
				if err != nil {
					return err
				}
				if perfJSON {
					return printJSON(st)
				}
				if st.On {
					fmt.Printf("Production-like mode: %s since %s%s\n", term.Green("on"), time.Unix(st.Since, 0).Format("Jan 2 15:04"),
						map[bool]string{true: " (config, routes, views and events cached)", false: ""}[st.Optimized])
				} else {
					fmt.Println("Production-like mode: off")
				}
				for _, n := range st.Notes {
					fmt.Println("  " + n)
				}
				return nil
			}
			return fmt.Errorf("usage: mullion perf %s prod on|off|status", site.Name)
		case "workers":
			return perfWorkers(a, site.Name)
		}
		return fmt.Errorf("unknown action %q — use routes, check, load, prod or workers", action)
	},
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func fmtBytesMB(n int64) string {
	if n <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func perfRoutes(a *app.App, site string) error {
	res, err := a.PerfRoutes(site)
	if err != nil {
		return err
	}
	if perfSaveBase != "" {
		note := ""
		if res.Prod {
			note = "production-like mode"
		}
		if _, err := perf.SaveBaseline(a.Paths, site, perfSaveBase, note, res.Routes); err != nil {
			return err
		}
		fmt.Println(term.Green("✓ Saved baseline " + perfSaveBase))
	}
	if perfBaseline != "" {
		b, err := perf.LoadBaseline(a.Paths, site, perfBaseline)
		if err != nil {
			return err
		}
		d := perf.Diff(res.Routes, b.Routes)
		if perfJSON {
			return printJSON(d)
		}
		fmt.Printf("Compared with baseline %q (%s)\n\n", b.Name, b.SavedAt.Local().Format("Jan 2 15:04"))
		for _, x := range d {
			line := fmt.Sprintf("  %-44s p95 %7.0f → %7.0f ms  %+6.1f%%   queries %5.1f → %5.1f", trunc(x.Key, 44), x.BaseP95Ms, x.P95Ms, x.P95Pct, x.BaseQ, x.Queries)
			switch {
			case x.Status == "new":
				line = fmt.Sprintf("  %-44s new: p95 %.0f ms", trunc(x.Key, 44), x.P95Ms)
			case x.Status == "gone":
				line = term.Dim(fmt.Sprintf("  %-44s not seen since", trunc(x.Key, 44)))
			case x.Regression:
				line = term.Red(line + "  regression")
			case x.Improvement:
				line = term.Green(line)
			}
			fmt.Println(line)
		}
		return nil
	}
	if perfJSON {
		return printJSON(res)
	}
	if !res.WatchOn {
		fmt.Println("The route table is built from the Watch tab's data — turn it on with: mullion watch " + site + " on")
		return nil
	}
	if len(res.Routes) == 0 {
		fmt.Println("No requests yet — use the site, then run this again.")
		return nil
	}
	fmt.Printf("%d requests · budgets: p95 %d ms, %d queries, %d MB%s\n\n", res.Requests, res.Budgets.MaxMs, res.Budgets.MaxQueries, res.Budgets.MaxMemoryMB,
		map[bool]string{true: "", false: "  (turn on Deep for queries and memory)"}[res.WatchDeep])
	fmt.Printf("  %-40s %6s %8s %8s %8s %8s %7s %9s %6s\n", "ROUTE", "COUNT", "AVG", "P50", "P95", "P99", "QUERIES", "MEMORY", "ERR")
	for _, r := range res.Routes {
		q := "—"
		if r.DeepCount > 0 {
			q = fmt.Sprintf("%.1f", r.AvgQueries)
		}
		line := fmt.Sprintf("  %-40s %6d %6.0fms %6.0fms %6.0fms %6.0fms %7s %9s %5.0f%%", trunc(r.Key, 40), r.Count, r.AvgMs, r.P50Ms, r.P95Ms, r.P99Ms, q, fmtBytesMB(r.PeakMemory), r.ErrorRate*100)
		if len(r.Over) > 0 {
			line = term.Yellow(line) + "\n" + term.Dim("      over budget: "+strings.Join(r.Over, "; "))
		}
		if r.NPlusOne > 0 {
			line += "\n" + term.Dim(fmt.Sprintf("      N+1 in %d request(s)", r.NPlusOne))
		}
		fmt.Println(line)
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func perfCheck(a *app.App, site string) error {
	fmt.Println("Checking " + site + "…")
	r, err := a.CheckReadiness(context.Background(), site)
	if err != nil {
		return err
	}
	if perfJSON {
		return printJSON(r)
	}
	col := map[string]func(string) string{perf.SevHigh: term.Red, perf.SevMedium: term.Yellow, perf.SevLow: term.Cyan, perf.SevInfo: term.Dim}
	shown := 0
	for _, f := range r.Findings {
		if f.Ignored && !perfShowIgnored {
			continue
		}
		shown++
		tag := col[f.Severity](fmt.Sprintf("[%s]", f.Severity))
		fmt.Printf("\n%s %s  %s\n", tag, term.Bold(f.Title), term.Dim(f.ID))
		fmt.Println("  " + f.Why)
		for _, w := range f.Where {
			fmt.Println("    " + w)
		}
		if f.Fix != "" {
			for _, l := range strings.Split(f.Fix, "\n") {
				fmt.Println(term.Cyan("    " + l))
			}
		}
	}
	for _, n := range r.Notes {
		fmt.Println("\n" + term.Dim(n))
	}
	ign := len(r.Findings) - shown
	fmt.Printf("\n%d finding(s)%s in %d ms.\n", shown, map[bool]string{true: fmt.Sprintf(" (%d ignored — --all shows them)", ign), false: ""}[ign > 0], r.Ms)
	return nil
}

func perfLoad(a *app.App, site, path string) error {
	headers := map[string]string{}
	for _, h := range perfHeaders {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return fmt.Errorf("a header is \"Name: value\", not %q", h)
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	in := app.LoadInput{URL: path, Method: perfMethod, Headers: headers, Concurrency: perfConc, Requests: perfRequests, WarmUpS: int(perfWarmUp / time.Second)}
	if perfRequests == 0 {
		in.DurationS = int(perfDuration / time.Second)
	}
	c, err := a.LoadConfig(site, in)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("Load test: %s %s · %d concurrent · %s — against your local database. Ctrl+C stops.\n", c.Method, c.URL, c.Concurrency,
		map[bool]string{true: fmt.Sprintf("%d requests", c.Requests), false: c.Duration.String()}[c.Duration == 0])
	isTTY := tty.IsTerminal(os.Stdout)
	snap, err := loadtest.Run(ctx, c, func(s loadtest.Snapshot) {
		if isTTY && !s.Done {
			fmt.Printf("\r  %-8s %6.1fs  %7d req  %7.1f rps  p50 %6.1f ms  p95 %6.1f ms  errors %d   ", s.Phase, s.Elapsed, s.Requests, s.RPS, s.P50Ms, s.P95Ms, s.Errors)
		}
	})
	if isTTY {
		fmt.Print("\r" + term.ClearLine())
	}
	if err != nil {
		return err
	}
	res, serr := a.SaveLoad(site, in, c, snap)
	if perfJSON {
		return printJSON(res)
	}
	fmt.Printf("%d requests in %.1fs — %.1f req/s, %d error(s)%s\n", snap.Requests, snap.Elapsed, snap.RPS, snap.Errors,
		map[bool]string{true: " (stopped)", false: ""}[snap.Phase == "stopped"])
	fmt.Printf("  latency  min %.1f · p50 %.1f · p90 %.1f · p95 %.1f · p99 %.1f · max %.1f ms\n", snap.MinMs, snap.P50Ms, snap.P90Ms, snap.P95Ms, snap.P99Ms, snap.MaxMs)
	var codes []string
	for k, v := range snap.Status {
		codes = append(codes, fmt.Sprintf("%s ×%d", k, v))
	}
	sort.Strings(codes)
	fmt.Println("  status   " + strings.Join(codes, " · "))
	for _, e := range snap.ErrorSamples {
		fmt.Println(term.Red("  error    " + e))
	}
	if serr == nil {
		if prev, ok := perf.Previous(perf.LoadResults(a.Paths, site, false), res); ok {
			cmp := perf.CompareLoad(res, prev)
			fmt.Printf("  vs previous (%s mode): rps %+.1f%% · p95 %+.1f%%\n", prev.Mode, cmp.RPSPct, cmp.P95Pct)
		}
	}
	return nil
}

func perfWorkers(a *app.App, site string) error {
	smp := workers.NewSampler(a.Paths, time.Second)
	var mine []workers.Target
	for _, t := range a.WorkerTargets() {
		if t.Site == site {
			mine = append(mine, t)
		}
	}
	if len(mine) > 0 {
		smp.SampleNow(mine)
	}
	list, err := a.WorkersHealth(site, nil)
	if err != nil {
		return err
	}
	if perfJSON {
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Println(site + " has no workers.")
		return nil
	}
	for _, w := range list {
		fmt.Printf("%s  %s  %s\n", term.Bold(w.Name), w.Status.State, term.Dim(w.Command))
		if n := len(w.Samples); n > 0 {
			last := w.Samples[n-1]
			fmt.Printf("  memory %s · cpu %.1f%% · %d process(es) · %d samples\n", fmtBytesMB(last.RSS), last.CPU, last.Procs, n)
		}
		fmt.Printf("  restarts %d", w.Status.Restarts)
		if w.Status.LastExit != "" {
			fmt.Printf(" · last exit: %s", w.Status.LastExit)
		}
		fmt.Println()
		leak := w.Leak.Reason
		if w.Leak.Suspected {
			leak = term.Red("possible leak: " + leak)
		}
		fmt.Println("  " + leak)
		if q := w.Queue; q != nil && len(q.Jobs) > 0 {
			for _, j := range q.Jobs {
				line := fmt.Sprintf("    %-40s done %d · failed %d · avg %.0f ms · max %.0f ms", trunc(j.Class, 40), j.Done, j.Failed, j.AvgMs, j.MaxMs)
				if j.Repeated {
					line = term.Red(line + " · failing repeatedly")
				}
				fmt.Println(line)
			}
		}
	}
	return nil
}

func init() {
	perfCmd.Flags().BoolVar(&perfJSON, "json", false, "print the result as JSON")
	perfCmd.Flags().IntVarP(&perfConc, "concurrency", "c", 10, "load: concurrent requests (1–200)")
	perfCmd.Flags().DurationVarP(&perfDuration, "duration", "d", 15*time.Second, "load: how long (5s–2m)")
	perfCmd.Flags().IntVarP(&perfRequests, "requests", "n", 0, "load: a number of requests instead of a duration")
	perfCmd.Flags().DurationVar(&perfWarmUp, "warmup", 2*time.Second, "load: warm-up not counted (0–30s)")
	perfCmd.Flags().StringVarP(&perfMethod, "method", "X", "GET", "load: the HTTP method")
	perfCmd.Flags().StringArrayVarP(&perfHeaders, "header", "H", nil, "load: a request header, \"Name: value\" (repeatable; e.g. a session Cookie)")
	perfCmd.Flags().BoolVar(&perfOptimize, "optimize", false, "prod on: also run `php artisan optimize` (undone by prod off)")
	perfCmd.Flags().StringVar(&perfSaveBase, "save", "", "routes: save the table as a baseline with this name")
	perfCmd.Flags().StringVar(&perfBaseline, "compare", "", "routes: compare with this baseline")
	perfCmd.Flags().BoolVar(&perfShowIgnored, "all", false, "check: include ignored findings")
	rootCmd.AddCommand(perfCmd)
}
