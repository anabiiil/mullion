package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"pm/internal/app"
	"pm/internal/term"
	"pm/internal/watch/explain"
)

// watchExplain prints a query's diagnosis and plan (`mullion watch
// <site> explain "<sql>"`).
func watchExplain(cmd *cobra.Command, a *app.App, site, sql string) error {
	in := app.WatchExplainInput{Conn: watchConn, Input: explain.Input{SQL: sql, Analyze: watchAnalyze}}
	if watchBindings != "" {
		if err := json.Unmarshal([]byte(watchBindings), &in.Bindings); err != nil {
			return fmt.Errorf("--bindings must be a JSON array, like '[42, \"a@b.test\"]': %v", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := a.WatchExplain(ctx, site, in)
	if err != nil {
		return err
	}
	if watchJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printExplain(res)
	return nil
}

var sevColor = map[string]func(string) string{"high": term.Red, "medium": term.Yellow, "low": term.Cyan, "info": term.Dim}

func printExplain(res *explain.Result) {
	head := res.Conn
	if res.Server != "" {
		head += " · " + res.Engine + " " + res.Server
	}
	if res.Plan.Analyzed {
		head += " · " + term.Yellow("analyzed (the query ran)")
	}
	fmt.Println(term.Dim(head))
	fmt.Println(res.BoundSQL)
	fmt.Println()
	if len(res.Findings) == 0 {
		fmt.Println(term.Green("✓ Nothing in the plan looks slow."))
	} else {
		fmt.Println(term.Bold("Findings"))
		for _, f := range res.Findings {
			c := sevColor[f.Severity]
			if c == nil {
				c = term.Dim
			}
			fmt.Printf("  %s %s\n", c(fmt.Sprintf("%-6s", strings.ToUpper(f.Severity))), f.Title)
			printWrapped("Why  ", f.Why)
			if f.Fix != "" {
				printWrapped("Fix  ", f.Fix)
			}
			if f.Code != "" {
				for _, l := range strings.Split(f.Code, "\n") {
					fmt.Println("         " + term.Cyan(l))
				}
			}
		}
	}
	if len(res.Suggestions) > 0 {
		fmt.Println()
		fmt.Println(term.Bold("Suggested indexes"))
		for _, s := range res.Suggestions {
			printWrapped("", s.Reason)
			fmt.Println("         " + term.Green(s.SQL))
			fmt.Println("         " + term.Dim("Laravel migration:"))
			for _, l := range strings.Split(s.Migration, "\n") {
				fmt.Println("         " + term.Cyan(l))
			}
		}
	}
	fmt.Println()
	fmt.Println(term.Bold("Plan"))
	if len(res.Plan.Tabular) > 0 {
		printTabular(res.Plan.Tabular)
	}
	if len(res.Plan.Tabular) == 0 || res.Plan.Analyzed {
		res.Plan.Walk(func(n *explain.Node, depth int) {
			if len(res.Plan.Tabular) > 0 && depth == 0 && n.Op != "EXPLAIN ANALYZE" {
				return
			}
			line := strings.Repeat("  ", depth+1) + "-> " + n.Op
			if n.Table != "" && !strings.Contains(n.Op, n.Table) {
				line += " on " + n.Table
			}
			if n.Index != "" && !strings.Contains(n.Op, n.Index) {
				line += " using " + n.Index
			}
			meta := ""
			if n.Rows > 0 {
				meta = fmt.Sprintf("rows≈%s", fmtNum(n.Rows))
			}
			if n.ActualRows >= 0 {
				meta += fmt.Sprintf(" actual=%s", fmtNum(n.ActualRows))
				meta = strings.TrimSpace(meta)
				if n.Loops > 1 {
					meta += fmt.Sprintf("×%s", fmtNum(n.Loops))
				}
			}
			if n.Cost > 0 {
				meta += fmt.Sprintf(" cost=%.1f", n.Cost)
			}
			fmt.Println(line + "  " + term.Dim(meta))
			if n.Detail != "" {
				fmt.Println(strings.Repeat("  ", depth+3) + term.Dim(n.Detail))
			}
		})
	}
	if len(res.Tables) > 0 {
		fmt.Println()
		fmt.Println(term.Bold("Indexes"))
		for name, t := range res.Tables {
			rows := "?"
			if t.Rows != nil {
				rows = "~" + fmtNum(*t.Rows)
			}
			fmt.Printf("  %s %s\n", name, term.Dim("("+rows+" rows)"))
			if len(t.Indexes) == 0 {
				fmt.Println("    " + term.Dim("no indexes"))
			}
			for _, ix := range t.Indexes {
				kind := ""
				if ix.Primary {
					kind = " primary"
				} else if ix.Unique {
					kind = " unique"
				}
				fmt.Printf("    %s (%s)%s\n", ix.Name, strings.Join(ix.Columns, ", "), term.Dim(kind))
			}
		}
	}
	for _, n := range res.Notes {
		fmt.Println()
		printWrapped("Note ", n)
	}
}

func fmtNum(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.2f", f)
}

func printTabular(rows []map[string]any) {
	cols := []string{"id", "select_type", "table", "type", "possible_keys", "key", "rows", "filtered", "Extra"}
	width := map[string]int{}
	cell := func(r map[string]any, c string) string {
		v := r[c]
		if v == nil {
			return "NULL"
		}
		if f, ok := v.(float64); ok {
			return fmtNum(f)
		}
		return fmt.Sprint(v)
	}
	for _, c := range cols {
		width[c] = len(c)
		for _, r := range rows {
			if l := len(cell(r, c)); l > width[c] {
				width[c] = l
			}
		}
	}
	line := func(get func(string) string) {
		var parts []string
		for _, c := range cols {
			parts = append(parts, fmt.Sprintf("%-*s", width[c], get(c)))
		}
		fmt.Println("  " + strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	line(func(c string) string { return c })
	for _, r := range rows {
		line(func(c string) string { return cell(r, c) })
	}
}

// printWrapped prints text wrapped at ~92 columns under a label.
func printWrapped(label, text string) {
	const width = 92
	indent := "         "
	words := strings.Fields(text)
	line := indent[:len(indent)-len(label)] + term.Dim(label)
	n := len(indent)
	first := true
	for _, w := range words {
		if !first && n+1+len(w) > width {
			fmt.Println(line)
			line, n = indent, len(indent)
			first = true
		}
		if !first {
			line += " "
			n++
		}
		line += w
		n += len(w)
		first = false
	}
	fmt.Println(line)
}
