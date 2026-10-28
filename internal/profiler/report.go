package profiler

// SPX "full" reports, as SPX stores them in spx.data_dir:
//
//	<key>.json    metadata (request, wall time, peak memory, call counts,
//	              the enabled metrics in SPX's metric order)
//	<key>.txt.gz  the trace: "[events]" lines "<function index> <1 start
//	              | 0 end> <metric values…>" — cumulative values since the
//	              profile began (wall time in ns, memory in bytes), one
//	              column per enabled metric — then "[functions]", one
//	              name per line (the index), "Class::method" or a file
//	              path for a script's top level.
//
// Note: SPX's "wall_time_ms" is microseconds (ns / 1000; its own UI
// multiplies it by 1000 to format nanoseconds).

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Meta is one report's metadata, as the profiles list shows it.
type Meta struct {
	ID         string   `json:"id"`
	Time       int64    `json:"time"` // unix seconds
	CLI        bool     `json:"cli"`
	Command    string   `json:"command,omitempty"`
	Method     string   `json:"method,omitempty"`
	URI        string   `json:"uri,omitempty"`
	Host       string   `json:"host,omitempty"`
	WallUs     int64    `json:"wallUs"`
	PeakMemory int64    `json:"peakMemory"`
	Functions  int64    `json:"functions"`
	Calls      int64    `json:"calls"`
	Recorded   int64    `json:"recorded"`
	Metrics    []string `json:"metrics"`
	Bytes      int64    `json:"bytes"`
}

// spxMeta is SPX's JSON layout.
type spxMeta struct {
	Key                 string   `json:"key"`
	ExecTS              int64    `json:"exec_ts"`
	CLI                 int      `json:"cli"`
	CLICommandLine      string   `json:"cli_command_line"`
	HTTPRequestURI      string   `json:"http_request_uri"`
	HTTPMethod          string   `json:"http_method"`
	HTTPHost            string   `json:"http_host"`
	WallTimeMs          int64    `json:"wall_time_ms"`
	PeakMemoryUsage     int64    `json:"peak_memory_usage"`
	CalledFunctionCount int64    `json:"called_function_count"`
	CallCount           int64    `json:"call_count"`
	RecordedCallCount   int64    `json:"recorded_call_count"`
	EnabledMetrics      []string `json:"enabled_metrics"`
}

// reportIDRe is what a report key may look like (it becomes a file name).
var reportIDRe = regexp.MustCompile(`^spx-full-[A-Za-z0-9._-]+$`)

// ValidID reports whether id can name a report file.
func ValidID(id string) bool { return reportIDRe.MatchString(id) && !strings.Contains(id, "..") }

func na(s string) string {
	if s == "n/a" {
		return ""
	}
	return s
}

// parseMeta decodes SPX's metadata JSON.
func parseMeta(data []byte) (Meta, error) {
	var m spxMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, err
	}
	if m.Key == "" {
		return Meta{}, errors.New("no report key")
	}
	return Meta{
		ID: m.Key, Time: m.ExecTS, CLI: m.CLI != 0, Command: na(m.CLICommandLine),
		Method: na(m.HTTPMethod), URI: na(m.HTTPRequestURI), Host: na(m.HTTPHost),
		WallUs: m.WallTimeMs, PeakMemory: m.PeakMemoryUsage,
		Functions: m.CalledFunctionCount, Calls: m.CallCount, Recorded: m.RecordedCallCount,
		Metrics: m.EnabledMetrics,
	}, nil
}

// List returns the reports in dir, newest first. Unreadable or
// half-written ones (no metadata yet) are skipped.
func List(dir string) ([]Meta, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "spx-full-*.json"))
	if err != nil {
		return nil, err
	}
	out := []Meta{}
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		m, err := parseMeta(data)
		if err != nil || !ValidID(m.ID) {
			continue
		}
		m.Bytes = int64(len(data))
		if info, err := os.Stat(traceFile(dir, m.ID)); err == nil {
			m.Bytes += info.Size()
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time > out[j].Time
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func traceFile(dir, id string) string { return filepath.Join(dir, id+".txt.gz") }
func metaFile(dir, id string) string  { return filepath.Join(dir, id+".json") }

// Get returns one report's metadata.
func Get(dir, id string) (Meta, error) {
	if !ValidID(id) {
		return Meta{}, fmt.Errorf("bad profile id %q", id)
	}
	data, err := os.ReadFile(metaFile(dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, fmt.Errorf("no profile %q", id)
	}
	if err != nil {
		return Meta{}, err
	}
	return parseMeta(data)
}

// Node is one flame-graph frame: a function reached through one call
// path, aggregated over every call along it. Times are nanoseconds.
type Node struct {
	Name     string  `json:"n"`
	Total    float64 `json:"v"`
	Self     float64 `json:"s"`
	Calls    int64   `json:"c"`
	Children []*Node `json:"ch,omitempty"`

	fn    int
	index map[int]*Node
}

// Func is one row of the top-functions table.
type Func struct {
	Name string `json:"name"`
	// Self is time spent in the function itself; Inclusive adds its
	// callees (recursive calls are counted once, at the outermost one).
	Self      float64 `json:"self"`
	Inclusive float64 `json:"inclusive"`
	Calls     int64   `json:"calls"`
	// Memory is the Zend memory the outermost calls left allocated
	// (bytes; can be negative).
	Memory float64 `json:"memory"`
}

// Profile is a parsed trace.
type Profile struct {
	Root  *Node  `json:"root"`
	Funcs []Func `json:"funcs"`
	// Events is how many trace lines were read.
	Events int64 `json:"events"`
	// Pruned counts flame-graph frames dropped as too small to draw.
	Pruned int `json:"pruned"`
}

type frame struct {
	node    *Node
	fn      int
	startWt float64
	startZm float64
	childWt float64
}

type funcAcc struct {
	self, inclusive, mem float64
	calls                int64
	active               int
}

// Parse reads a trace (gzip-compressed or not) recorded with the given
// metrics (Meta.Metrics, in SPX's order) and aggregates it by call
// stack and by function.
func Parse(r io.Reader, metrics []string) (*Profile, error) {
	wtCol, zmCol := -1, -1
	for i, m := range metrics {
		switch m {
		case "wt":
			wtCol = i
		case "zm":
			zmCol = i
		}
	}
	if wtCol < 0 {
		return nil, errors.New("this profile has no wall-time metric")
	}

	br := bufio.NewReaderSize(r, 1<<16)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		br = bufio.NewReaderSize(gz, 1<<16)
	}

	root := &Node{Name: "all", fn: -1}
	stack := []frame{{node: root, fn: -1}}
	accs := map[int]*funcAcc{}
	acc := func(fn int) *funcAcc {
		a := accs[fn]
		if a == nil {
			a = &funcAcc{}
			accs[fn] = a
		}
		return a
	}
	var names []string
	var events int64
	var lastWt float64
	section := ""
	fields := make([]string, 0, 8)

	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		switch section {
		case "[functions]":
			names = append(names, line)
		case "[events]":
			fields = splitFields(line, fields[:0])
			if len(fields) < 2+len(metrics) {
				continue
			}
			fn, err := strconv.Atoi(fields[0])
			if err != nil {
				continue
			}
			wt, _ := strconv.ParseFloat(fields[2+wtCol], 64)
			var zm float64
			if zmCol >= 0 {
				zm, _ = strconv.ParseFloat(fields[2+zmCol], 64)
			}
			events++
			lastWt = wt
			if fields[1] == "1" {
				parent := stack[len(stack)-1].node
				child := parent.child(fn)
				acc(fn).active++
				stack = append(stack, frame{node: child, fn: fn, startWt: wt, startZm: zm})
				continue
			}
			// An end: close frames up to the matching start (SPX nests
			// them properly; this only guards a truncated trace).
			for len(stack) > 1 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				closeFrame(f, &stack[len(stack)-1], wt, zm, acc(f.fn))
				if f.fn == fn {
					break
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the trace: %w", err)
	}
	// A trace cut short (a crashed request): close what is still open.
	for len(stack) > 1 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		closeFrame(f, &stack[len(stack)-1], lastWt, f.startZm, acc(f.fn))
	}

	name := func(fn int) string {
		if fn >= 0 && fn < len(names) {
			return names[fn]
		}
		return fmt.Sprintf("#%d", fn)
	}
	for _, c := range root.Children {
		root.Total += c.Total
	}
	root.finish(name)

	p := &Profile{Root: root, Events: events}
	for fn, a := range accs {
		p.Funcs = append(p.Funcs, Func{Name: name(fn), Self: a.self, Inclusive: a.inclusive, Calls: a.calls, Memory: a.mem})
	}
	sort.Slice(p.Funcs, func(i, j int) bool {
		if p.Funcs[i].Self != p.Funcs[j].Self {
			return p.Funcs[i].Self > p.Funcs[j].Self
		}
		return p.Funcs[i].Name < p.Funcs[j].Name
	})
	return p, nil
}

// closeFrame books an ended call into its node, its parent and its
// function's totals.
func closeFrame(f frame, parent *frame, wt, zm float64, a *funcAcc) {
	dt := wt - f.startWt
	if dt < 0 {
		dt = 0
	}
	self := dt - f.childWt
	if self < 0 {
		self = 0
	}
	f.node.Total += dt
	f.node.Self += self
	f.node.Calls++
	parent.childWt += dt
	a.self += self
	a.calls++
	a.active--
	if a.active <= 0 {
		a.active = 0
		a.inclusive += dt
		a.mem += zm - f.startZm
	}
}

// child returns (creating it) n's child frame for fn.
func (n *Node) child(fn int) *Node {
	if n.index == nil {
		n.index = map[int]*Node{}
	}
	c := n.index[fn]
	if c == nil {
		c = &Node{fn: fn}
		n.index[fn] = c
		n.Children = append(n.Children, c)
	}
	return c
}

// finish names every node and sorts children by time, biggest first.
func (n *Node) finish(name func(int) string) {
	todo := []*Node{n}
	for len(todo) > 0 {
		x := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if x.fn >= 0 {
			x.Name = name(x.fn)
		}
		x.index = nil
		sort.SliceStable(x.Children, func(i, j int) bool { return x.Children[i].Total > x.Children[j].Total })
		todo = append(todo, x.Children...)
	}
}

// Prune drops frames below minFrac of the root's time (too narrow to
// draw), counting them in Pruned, keeping the JSON small for
// reports with millions of calls.
func (p *Profile) Prune(minFrac float64) {
	if p.Root == nil || p.Root.Total <= 0 || minFrac <= 0 {
		return
	}
	min := p.Root.Total * minFrac
	todo := []*Node{p.Root}
	for len(todo) > 0 {
		x := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		kept := x.Children[:0]
		for _, c := range x.Children {
			if c.Total >= min {
				kept = append(kept, c)
			} else {
				p.Pruned += c.count()
			}
		}
		x.Children = kept
		todo = append(todo, kept...)
	}
}

func (n *Node) count() int {
	c := 1
	for _, ch := range n.Children {
		c += ch.count()
	}
	return c
}

// LoadProfile parses a stored report's trace.
func LoadProfile(dir, id string) (Meta, *Profile, error) {
	m, err := Get(dir, id)
	if err != nil {
		return m, nil, err
	}
	f, err := os.Open(traceFile(dir, id))
	if err != nil {
		return m, nil, fmt.Errorf("profile %s has no trace: %w", id, err)
	}
	defer f.Close()
	p, err := Parse(f, m.Metrics)
	return m, p, err
}

// splitFields is strings.Fields into a reused slice.
func splitFields(s string, out []string) []string {
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' || s[i] == '\r' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}
