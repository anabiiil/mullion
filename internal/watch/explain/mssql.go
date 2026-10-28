package explain

// SQL Server's SHOWPLAN_XML: RelOp elements (PhysicalOp, EstimateRows,
// the operator's <Object> for table and index) nested in each other,
// plus <Warnings> (implicit conversions, spills) and — the best part —
// <MissingIndexes>, the optimizer's own index suggestion.

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	reCost   = regexp.MustCompile(`cost=([\d.e+]+)\s+rows=([\d.e+]+)`)
	reActual = regexp.MustCompile(`actual time=([\d.e+]+)\.\.([\d.e+]+)\s+rows=([\d.e+]+)\s+loops=([\d.e+]+)`)
)

type xnode struct {
	Name  string
	Attrs map[string]string
	Kids  []*xnode
}

func (x *xnode) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	x.Name = start.Name.Local
	x.Attrs = map[string]string{}
	for _, a := range start.Attr {
		x.Attrs[a.Name.Local] = a.Value
	}
	for {
		t, err := d.Token()
		if err != nil {
			return err
		}
		switch e := t.(type) {
		case xml.StartElement:
			k := &xnode{}
			if err := k.UnmarshalXML(d, e); err != nil {
				return err
			}
			x.Kids = append(x.Kids, k)
		case xml.EndElement:
			return nil
		}
	}
}

func (x *xnode) find(fn func(*xnode) bool) []*xnode {
	var out []*xnode
	var rec func(n *xnode)
	rec = func(n *xnode) {
		if fn(n) {
			out = append(out, n)
		}
		for _, k := range n.Kids {
			rec(k)
		}
	}
	rec(x)
	return out
}

func unbracket(s string) string { return strings.Trim(s, "[]") }

// mssqlPlan parses a ShowPlanXML document.
func mssqlPlan(raw string) ([]*Node, []MissingIndex, []Warning, float64, error) {
	var root xnode
	dec := xml.NewDecoder(strings.NewReader(raw))
	// The plan text is UTF-8 whatever its declaration says (utf-16).
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&root); err != nil {
		return nil, nil, nil, 0, fmt.Errorf("unreadable SQL Server plan: %v", err)
	}
	var missing []MissingIndex
	for _, g := range root.find(func(n *xnode) bool { return n.Name == "MissingIndexGroup" }) {
		impact := toFloat(g.Attrs["Impact"])
		for _, mi := range g.find(func(n *xnode) bool { return n.Name == "MissingIndex" }) {
			m := MissingIndex{Table: unbracket(mi.Attrs["Table"]), Impact: impact}
			if s := unbracket(mi.Attrs["Schema"]); s != "" && s != "dbo" {
				m.Table = s + "." + m.Table
			}
			for _, cg := range mi.Kids {
				if cg.Name != "ColumnGroup" {
					continue
				}
				var cols []string
				for _, c := range cg.Kids {
					if c.Name == "Column" {
						cols = append(cols, unbracket(c.Attrs["Name"]))
					}
				}
				switch cg.Attrs["Usage"] {
				case "EQUALITY":
					m.Equality = append(m.Equality, cols...)
				case "INEQUALITY":
					m.Inequality = append(m.Inequality, cols...)
				case "INCLUDE":
					m.Include = append(m.Include, cols...)
				}
			}
			missing = append(missing, m)
		}
	}
	var warns []Warning
	for _, w := range root.find(func(n *xnode) bool { return n.Name == "Warnings" }) {
		for _, k := range w.Kids {
			msg := k.Name
			switch k.Name {
			case "PlanAffectingConvert":
				msg = "Implicit conversion (" + k.Attrs["ConvertIssue"] + "): " + k.Attrs["Expression"]
			case "SpillToTempDb":
				msg = "Spilled to tempdb (level " + k.Attrs["SpillLevel"] + ")"
			case "ColumnsWithNoStatistics":
				var cols []string
				for _, c := range k.find(func(n *xnode) bool { return n.Name == "ColumnReference" }) {
					cols = append(cols, unbracket(c.Attrs["Column"]))
				}
				msg = "Columns with no statistics: " + strings.Join(cols, ", ")
			case "NoJoinPredicate":
				msg = "No join predicate (a cross join)"
			}
			warns = append(warns, Warning{Code: k.Name, Message: msg})
		}
	}
	if w := root.find(func(n *xnode) bool { return n.Name == "Warnings" && n.Attrs["NoJoinPredicate"] == "true" }); len(w) > 0 {
		warns = append(warns, Warning{Code: "NoJoinPredicate", Message: "No join predicate (a cross join)"})
	}
	cost := 0.0
	var rec func(x *xnode) []*Node
	rec = func(x *xnode) []*Node {
		var out []*Node
		for _, k := range x.Kids {
			if k.Name != "RelOp" {
				out = append(out, rec(k)...)
				continue
			}
			op := k.Attrs["PhysicalOp"]
			n := newNode(op)
			n.Rows = toFloat(k.Attrs["EstimateRows"])
			// Inner side of a nested loop: EstimateRows is per execution.
			if rb := toFloat(k.Attrs["EstimateRebinds"]) + toFloat(k.Attrs["EstimateRewinds"]); rb > 0 {
				n.Loops = rb + 1
			}
			n.Cost = toFloat(k.Attrs["EstimatedTotalSubtreeCost"])
			if cost == 0 {
				cost = n.Cost
			}
			// The operator's own element holds <Object> (its table and index).
			for _, sub := range k.Kids {
				if sub.Name == "RelOp" || sub.Name == "OutputList" || sub.Name == "Warnings" || sub.Name == "MemoryFractions" || sub.Name == "RunTimeInformation" {
					continue
				}
				for _, o := range sub.Kids {
					if o.Name == "Object" && n.Table == "" {
						n.Table = unbracket(o.Attrs["Table"])
						n.Index = unbracket(o.Attrs["Index"])
					}
					if o.Name == "Predicate" || o.Name == "SeekPredicates" {
						for _, so := range o.find(func(n *xnode) bool { return n.Name == "ScalarOperator" }) {
							if s := so.Attrs["ScalarString"]; s != "" {
								n.Detail = o.Name + ": " + s
								break
							}
						}
					}
				}
			}
			for _, w := range k.Kids {
				if w.Name == "Warnings" {
					for _, ww := range w.Kids {
						n.Extra = append(n.Extra, ww.Name)
						if ww.Name == "SpillToTempDb" {
							if op == "Sort" {
								n.DiskSort = true
							} else {
								n.DiskHash = true
							}
						}
					}
				}
			}
			switch op {
			case "Table Scan", "Clustered Index Scan":
				n.FullScan = true
			case "Index Scan":
				n.IndexScan = true
			case "Sort":
				n.Filesort = true
			case "Key Lookup", "RID Lookup":
				n.Lookup = true
			case "Table Spool", "Index Spool":
				n.Temporary = true
			}
			n.Children = rec(k)
			out = append(out, n)
		}
		return out
	}
	nodes := rec(&root)
	return nodes, missing, warns, cost, nil
}
