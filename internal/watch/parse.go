package watch

import (
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Item is one thing the watcher shows: a request, a log entry, a query…
// Every item has "type", "t" (unix seconds, float) and, when known,
// "rid" (the request — or job — it belongs to). The rest depends on the
// type; the panel reads it as is.
type Item map[string]any

func (it Item) Type() string { s, _ := it["type"].(string); return s }
func (it Item) Time() float64 {
	switch v := it["t"].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}
func (it Item) RID() string { s, _ := it["rid"].(string); return s }

// ── Caddy access log ─────────────────────────────────────────────────

type caddyEntry struct {
	TS      float64 `json:"ts"`
	Msg     string  `json:"msg"`
	Logger  string  `json:"logger"`
	Request struct {
		RemoteIP string              `json:"remote_ip"`
		ClientIP string              `json:"client_ip"`
		Proto    string              `json:"proto"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
	Duration    float64             `json:"duration"`
	Size        int64               `json:"size"`
	Status      int                 `json:"status"`
	RespHeaders map[string][]string `json:"resp_headers"`
	RID         string              `json:"mullion_rid"`
}

// staticExt are the extensions "Exclude static assets" hides.
var staticExt = map[string]bool{
	".css": true, ".js": true, ".mjs": true, ".map": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".svg": true, ".webp": true, ".avif": true, ".ico": true, ".woff": true, ".woff2": true,
	".ttf": true, ".otf": true, ".eot": true, ".mp4": true, ".webm": true, ".mp3": true, ".txt": true,
	".json": false, ".xml": true, ".pdf": true, ".wasm": true,
}

// IsStatic reports whether a request path looks like a static asset.
func IsStatic(p string) bool {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if strings.HasPrefix(p, "/build/") || strings.HasPrefix(p, "/vendor/") || strings.HasPrefix(p, "/@vite/") {
		return true
	}
	return staticExt[strings.ToLower(path.Ext(p))]
}

// ParseCaddyLine turns one line of a site's JSON access log into a
// "request" item (ok=false for anything else: Caddy's own errors, a
// non-JSON line). Secret headers are redacted (Caddy already blanks
// Cookie and Authorization).
func ParseCaddyLine(line string) (Item, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil, false
	}
	var e caddyEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return nil, false
	}
	if e.Request.Method == "" || e.Status == 0 && e.Msg != "handled request" {
		return nil, false
	}
	status := e.Status
	if status == 0 {
		status = 200 // Caddy logs 0 when the handler wrote no header
	}
	p, q := e.Request.URI, ""
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p, q = p[:i], p[i+1:]
	}
	it := Item{
		"type":    "request",
		"src":     "caddy",
		"t":       e.TS,
		"method":  e.Request.Method,
		"path":    p,
		"uri":     e.Request.URI,
		"status":  status,
		"ms":      round2(e.Duration * 1000),
		"size":    e.Size,
		"host":    e.Request.Host,
		"proto":   e.Request.Proto,
		"ip":      firstNonEmpty(e.Request.ClientIP, e.Request.RemoteIP),
		"static":  IsStatic(p),
		"headers": RedactHeaders(e.Request.Headers),
	}
	if q != "" {
		if vals, err := url.ParseQuery(q); err == nil {
			it["query"] = RedactQuery(vals)
		}
	}
	if len(e.RespHeaders) > 0 {
		it["respHeaders"] = RedactHeaders(e.RespHeaders)
	}
	if e.RID != "" {
		it["rid"] = e.RID
	}
	return it, true
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── Monolog (Laravel's storage/logs/*.log) ───────────────────────────

// monologHead matches the first line of an entry:
// [2026-10-21 19:00:37] local.ERROR: message {context} [extra]
var monologHead = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:?\d{2}|Z)?)\] ([\w.-]+)\.([A-Z]+): ?(.*)$`)

// exceptionHead matches Laravel's rendering of an exception in the
// context: {"exception":"[object] (Class(code: 0): message at /file.php:12)
var exceptionHead = regexp.MustCompile(`"exception":"\[object\] \(([^\s(]+)\(code: [^)]*\): (.*) at (.+?):(\d+)\)\s*$`)

// frameLine matches one stack frame: #3 /path/file.php(42): Class->method()
var frameLine = regexp.MustCompile(`^#(\d+) (?:(.+?)\((\d+)\)|\[internal function\]): (.*)$`)

// LogEntry is one Monolog record, possibly spanning many lines.
type LogEntry struct {
	Time    time.Time
	Env     string
	Level   string
	Message string
	// Exception, File, Line and Trace are set when the entry carries one.
	Exception string
	File      string
	Line      int
	Trace     []Frame
	Raw       string
}

// Frame is one stack-trace frame.
type Frame struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Call string `json:"call"`
}

// MonologParser assembles multi-line Monolog entries from lines fed in
// order. Lines before the first header are dropped.
type MonologParser struct {
	cur   *LogEntry
	lines []string
	// Loc is the timezone of timestamps without an offset (Laravel
	// writes app.timezone, usually UTC).
	Loc *time.Location
}

const maxEntryLines = 400

// Feed adds one line; it returns the entry the line completed (a new
// header ends the previous entry), or nil.
func (p *MonologParser) Feed(line string) *LogEntry {
	if m := monologHead.FindStringSubmatch(line); m != nil {
		done := p.Flush()
		p.cur = &LogEntry{Time: parseLogTime(m[1], p.Loc), Env: m[2], Level: m[3]}
		p.lines = []string{m[4]}
		return done
	}
	if p.cur != nil && len(p.lines) < maxEntryLines {
		p.lines = append(p.lines, line)
	}
	return nil
}

// Flush finishes the pending entry (call when no more lines follow for
// a while, e.g. the file stopped growing).
func (p *MonologParser) Flush() *LogEntry {
	e := p.cur
	if e == nil {
		return nil
	}
	p.cur = nil
	lines := p.lines
	p.lines = nil
	e.Raw = strings.Join(lines, "\n")
	first := lines[0]
	if m := exceptionHead.FindStringSubmatch(first); m != nil {
		e.Exception = strings.ReplaceAll(m[1], `\\`, `\`)
		e.File = unescapeJSONish(m[3])
		e.Line, _ = strconv.Atoi(m[4])
		// The message ends where the context object opens.
		head := first[:len(first)-len(m[0])]
		if i := strings.LastIndex(head, ` {"`); i >= 0 {
			head = head[:i]
		} else {
			head = strings.TrimSuffix(head, "{")
		}
		e.Message = strings.TrimSpace(head)
		if e.Message == "" {
			e.Message = unescapeJSONish(m[2])
		}
		for _, l := range lines[1:] {
			if f := frameLine.FindStringSubmatch(strings.TrimSpace(l)); f != nil {
				ln, _ := strconv.Atoi(f[3])
				e.Trace = append(e.Trace, Frame{File: unescapeJSONish(f[2]), Line: ln, Call: unescapeJSONish(f[4])})
			}
		}
		return e
	}
	e.Message = strings.TrimRight(strings.Join(lines, "\n"), " \n")
	// Drop an empty trailing "[] []" context/extra.
	e.Message = strings.TrimSuffix(e.Message, " [] []")
	return e
}

// Pending reports whether an entry is waiting for more lines.
func (p *MonologParser) Pending() bool { return p.cur != nil }

func parseLogTime(s string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t
		}
	}
	return time.Now()
}

// unescapeJSONish undoes the escaping Monolog's JSON context applies to
// a message or path (\/ and \\).
func unescapeJSONish(s string) string {
	s = strings.ReplaceAll(s, `\/`, `/`)
	return strings.ReplaceAll(s, `\\`, `\`)
}

// Item renders a log entry for the panel: a "log" item, or an
// "exception" one (with a group key: same class at the same place).
func (e *LogEntry) Item(file string) Item {
	it := Item{
		"type":    "log",
		"src":     "file",
		"t":       float64(e.Time.UnixMilli()) / 1000,
		"level":   strings.ToLower(e.Level),
		"env":     e.Env,
		"message": truncate(e.Message, 8000),
		"logFile": file,
	}
	if e.Exception != "" {
		it["type"] = "exception"
		it["class"] = e.Exception
		it["file"] = e.File
		it["line"] = e.Line
		it["trace"] = e.Trace
		it["group"] = e.Exception + "@" + e.File + ":" + strconv.Itoa(e.Line)
	}
	return it
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
