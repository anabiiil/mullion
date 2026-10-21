package redis

// A minimal RESP2 client — just what the panel and the CLI need to look
// at a local dev Redis (INFO, SCAN, TYPE/TTL/MEMORY USAGE, bounded value
// previews, UNLINK, FLUSHDB, SAVE, MONITOR) without a third-party
// module. Every command is sent as an array of bulk strings, so keys
// and values are binary-safe.
//
// Nothing here ever issues KEYS: listing keys always goes through SCAN
// with MATCH, which never blocks the server.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Value is one RESP reply.
type Value struct {
	// Kind is the RESP type byte: '+' status, '-' error, ':' integer,
	// '$' bulk string, '*' array.
	Kind  byte
	Str   string // status, error and bulk replies
	Int   int64  // integer replies
	Array []Value
	// Nil marks a null bulk string or null array ($-1 / *-1).
	Nil bool
}

// Error is an error reply from the server ("-ERR ...", "-WRONGTYPE ...").
type Error string

func (e Error) Error() string { return "redis: " + string(e) }

// ErrProtocol reports a reply that isn't valid RESP.
var ErrProtocol = errors.New("redis: protocol error")

// maxBulk bounds one bulk reply — previews are bounded by the commands
// that produce them, so anything bigger is a protocol error, not data.
const maxBulk = 64 << 20

// Client is one connection. It is not safe for concurrent use.
type Client struct {
	conn    net.Conn
	r       *bufio.Reader
	timeout time.Duration
}

// panelAddrs records the local addresses of the connections this
// process opened, so a MONITOR stream can leave out the panel's own
// commands (its SCANs and INFOs would otherwise flood the monitor).
var panelAddrs sync.Map

// Dial connects to addr ("127.0.0.1:6379"); timeout bounds the dial and
// every later command (0 = 5s).
func Dial(addr string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	panelAddrs.Store(conn.LocalAddr().String(), true)
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 32<<10), timeout: timeout}, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	panelAddrs.Delete(c.conn.LocalAddr().String())
	return c.conn.Close()
}

// IsPanelAddr reports whether a client address (as MONITOR prints it)
// is one of this process's own connections.
func IsPanelAddr(addr string) bool {
	_, ok := panelAddrs.Load(addr)
	return ok
}

// writeCommand encodes args as a RESP array of bulk strings.
func writeCommand(w io.Writer, args []string) error {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n")
		b.WriteString(a)
		b.WriteString("\r\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// readLine reads one CRLF-terminated line, without the CRLF.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", ErrProtocol
	}
	return line[:len(line)-2], nil
}

// readValue parses one reply.
func readValue(r *bufio.Reader) (Value, error) {
	line, err := readLine(r)
	if err != nil {
		return Value{}, err
	}
	if line == "" {
		return Value{}, ErrProtocol
	}
	kind, rest := line[0], line[1:]
	switch kind {
	case '+', '-':
		return Value{Kind: kind, Str: rest}, nil
	case ':':
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return Value{}, ErrProtocol
		}
		return Value{Kind: kind, Int: n}, nil
	case '$':
		n, err := strconv.Atoi(rest)
		if err != nil || n > maxBulk {
			return Value{}, ErrProtocol
		}
		if n < 0 {
			return Value{Kind: kind, Nil: true}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Value{}, err
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return Value{}, ErrProtocol
		}
		return Value{Kind: kind, Str: string(buf[:n])}, nil
	case '*':
		n, err := strconv.Atoi(rest)
		if err != nil || n > 1<<20 {
			return Value{}, ErrProtocol
		}
		if n < 0 {
			return Value{Kind: kind, Nil: true}, nil
		}
		arr := make([]Value, n)
		for i := range arr {
			if arr[i], err = readValue(r); err != nil {
				return Value{}, err
			}
		}
		return Value{Kind: kind, Array: arr}, nil
	}
	return Value{}, ErrProtocol
}

// Do sends one command and reads its reply. An error reply comes back
// as an Error.
func (c *Client) Do(args ...string) (Value, error) {
	c.conn.SetDeadline(time.Now().Add(c.timeout))
	if err := writeCommand(c.conn, args); err != nil {
		return Value{}, err
	}
	v, err := readValue(c.r)
	if err != nil {
		return Value{}, err
	}
	if v.Kind == '-' {
		return v, Error(v.Str)
	}
	return v, nil
}

// DoMulti pipelines several commands — all written, then all replies
// read — so per-key lookups over a page of keys cost one round trip.
// Each reply's error (an error reply) is in errs at the same index; err
// is a connection failure.
func (c *Client) DoMulti(cmds [][]string) (vals []Value, errs []error, err error) {
	if len(cmds) == 0 {
		return nil, nil, nil
	}
	c.conn.SetDeadline(time.Now().Add(c.timeout + time.Duration(len(cmds))*time.Millisecond))
	var b strings.Builder
	for _, args := range cmds {
		if err := writeCommand(&b, args); err != nil {
			return nil, nil, err
		}
	}
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return nil, nil, err
	}
	vals = make([]Value, len(cmds))
	errs = make([]error, len(cmds))
	for i := range cmds {
		v, err := readValue(c.r)
		if err != nil {
			return nil, nil, err
		}
		vals[i] = v
		if v.Kind == '-' {
			errs[i] = Error(v.Str)
		}
	}
	return vals, errs, nil
}

// String is a reply as text (status, bulk, or an integer's digits).
func (v Value) String() string {
	if v.Kind == ':' {
		return strconv.FormatInt(v.Int, 10)
	}
	return v.Str
}

// Strings flattens an array reply of bulk strings.
func (v Value) Strings() []string {
	out := make([]string, 0, len(v.Array))
	for _, e := range v.Array {
		out = append(out, e.String())
	}
	return out
}

func (c *Client) intCmd(args ...string) (int64, error) {
	v, err := c.Do(args...)
	if err != nil {
		return 0, err
	}
	if v.Kind != ':' {
		return 0, ErrProtocol
	}
	return v.Int, nil
}

// Auth logs in (password only, the default user).
func (c *Client) Auth(password string) error {
	_, err := c.Do("AUTH", password)
	return err
}

// Select switches the connection to a numbered database.
func (c *Client) Select(db int) error {
	_, err := c.Do("SELECT", strconv.Itoa(db))
	return err
}

// Ping checks the connection.
func (c *Client) Ping() error {
	_, err := c.Do("PING")
	return err
}

// Info returns INFO [section] parsed into field -> value.
func (c *Client) Info(section string) (map[string]string, error) {
	args := []string{"INFO"}
	if section != "" {
		args = append(args, section)
	}
	v, err := c.Do(args...)
	if err != nil {
		return nil, err
	}
	return ParseInfo(v.Str), nil
}

// ParseInfo parses INFO's "field:value" lines (section headers and
// blank lines are skipped).
func ParseInfo(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			out[k] = v
		}
	}
	return out
}

// KeyspaceDB is one "dbN:keys=..,expires=..,avg_ttl=.." line of INFO
// keyspace.
type KeyspaceDB struct {
	DB      int   `json:"db"`
	Keys    int64 `json:"keys"`
	Expires int64 `json:"expires"`
	AvgTTL  int64 `json:"avgTtl"`
}

// ParseKeyspace extracts the per-database key counts from parsed INFO,
// sorted by database number.
func ParseKeyspace(info map[string]string) []KeyspaceDB {
	var out []KeyspaceDB
	for k, v := range info {
		if !strings.HasPrefix(k, "db") {
			continue
		}
		n, err := strconv.Atoi(k[2:])
		if err != nil {
			continue
		}
		d := KeyspaceDB{DB: n}
		for _, part := range strings.Split(v, ",") {
			name, val, _ := strings.Cut(part, "=")
			x, _ := strconv.ParseInt(val, 10, 64)
			switch name {
			case "keys":
				d.Keys = x
			case "expires":
				d.Expires = x
			case "avg_ttl":
				d.AvgTTL = x
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DB < out[j].DB })
	return out
}

// Scan runs one SCAN step: keys matching the glob pattern (""
// = all), count a hint of how much work to do, typ an optional TYPE
// filter. It returns the next cursor (0 = done).
func (c *Client) Scan(cursor uint64, match string, count int, typ string) (uint64, []string, error) {
	args := []string{"SCAN", strconv.FormatUint(cursor, 10)}
	if match != "" {
		args = append(args, "MATCH", match)
	}
	if count > 0 {
		args = append(args, "COUNT", strconv.Itoa(count))
	}
	if typ != "" {
		args = append(args, "TYPE", typ)
	}
	v, err := c.Do(args...)
	if err != nil {
		return 0, nil, err
	}
	if v.Kind != '*' || len(v.Array) != 2 {
		return 0, nil, ErrProtocol
	}
	next, err := strconv.ParseUint(v.Array[0].Str, 10, 64)
	if err != nil {
		return 0, nil, ErrProtocol
	}
	return next, v.Array[1].Strings(), nil
}

// Type is TYPE key ("string", "hash", ..., "none").
func (c *Client) Type(key string) (string, error) {
	v, err := c.Do("TYPE", key)
	return v.Str, err
}

// PTTL is the key's time to live in milliseconds: -1 = no expiry,
// -2 = no such key.
func (c *Client) PTTL(key string) (int64, error) { return c.intCmd("PTTL", key) }

// MemoryUsage is MEMORY USAGE key in bytes (0 when the key is gone).
func (c *Client) MemoryUsage(key string) (int64, error) {
	v, err := c.Do("MEMORY", "USAGE", key, "SAMPLES", "5")
	if err != nil {
		return 0, err
	}
	return v.Int, nil
}

// Unlink deletes keys without blocking the server (the memory is freed
// in the background), returning how many existed.
func (c *Client) Unlink(keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	return c.intCmd(append([]string{"UNLINK"}, keys...)...)
}

// DBSize is the number of keys in the selected database.
func (c *Client) DBSize() (int64, error) { return c.intCmd("DBSIZE") }

// FlushDB empties the selected database (asynchronously on the server).
func (c *Client) FlushDB() error {
	_, err := c.Do("FLUSHDB", "ASYNC")
	return err
}

// FlushAll empties every database.
func (c *Client) FlushAll() error {
	_, err := c.Do("FLUSHALL", "ASYNC")
	return err
}

// Save writes a snapshot (dump.rdb) synchronously.
func (c *Client) Save() error {
	_, err := c.Do("SAVE")
	return err
}

// ConfigGet returns one CONFIG GET parameter's value ("" when unknown).
func (c *Client) ConfigGet(param string) (string, error) {
	v, err := c.Do("CONFIG", "GET", param)
	if err != nil {
		return "", err
	}
	if len(v.Array) >= 2 {
		return v.Array[1].String(), nil
	}
	return "", nil
}

// Shutdown asks the server to save and exit. The server closes the
// connection instead of replying, so EOF is success.
func (c *Client) Shutdown() error {
	c.conn.SetDeadline(time.Now().Add(c.timeout))
	if err := writeCommand(c.conn, []string{"SHUTDOWN", "SAVE"}); err != nil {
		return err
	}
	v, err := readValue(c.r)
	if err == nil && v.Kind == '-' {
		return Error(v.Str)
	}
	return nil
}

/* ── value previews ─────────────────────────────────────────── */

// Field is one entry of a hash, or a member of a sorted set (Value is
// then its score).
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Preview is a bounded look at one key's value.
type Preview struct {
	Key  string `json:"key"`
	Type string `json:"type"`
	// TTL in milliseconds (-1 = none).
	TTL int64 `json:"ttl"`
	// Size is the value's length: bytes for a string, elements for the
	// collections.
	Size int64 `json:"size"`
	// Memory is MEMORY USAGE in bytes.
	Memory int64 `json:"memory"`
	// Value is a string's (first maxBytes) bytes.
	Value string `json:"value,omitempty"`
	// Items are a list's, set's or stream's first elements; Fields a
	// hash's or sorted set's.
	Items  []string `json:"items,omitempty"`
	Fields []Field  `json:"fields,omitempty"`
	// Truncated: there is more than the preview shows.
	Truncated bool `json:"truncated"`
}

// GetPreview reads at most limit elements (and maxBytes of a string)
// of a key — never the whole of a big value.
func (c *Client) GetPreview(key string, limit, maxBytes int) (Preview, error) {
	if limit <= 0 {
		limit = 100
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	p := Preview{Key: key}
	var err error
	if p.Type, err = c.Type(key); err != nil {
		return p, err
	}
	if p.Type == "none" {
		return p, Error("no such key")
	}
	p.TTL, _ = c.PTTL(key)
	p.Memory, _ = c.MemoryUsage(key)
	lim := strconv.Itoa(limit - 1)
	switch p.Type {
	case "string":
		if p.Size, err = c.intCmd("STRLEN", key); err != nil {
			return p, err
		}
		v, err := c.Do("GETRANGE", key, "0", strconv.Itoa(maxBytes-1))
		if err != nil {
			return p, err
		}
		p.Value = v.Str
		p.Truncated = p.Size > int64(maxBytes)
	case "list":
		if p.Size, err = c.intCmd("LLEN", key); err != nil {
			return p, err
		}
		v, err := c.Do("LRANGE", key, "0", lim)
		if err != nil {
			return p, err
		}
		p.Items = v.Strings()
	case "set":
		if p.Size, err = c.intCmd("SCARD", key); err != nil {
			return p, err
		}
		v, err := c.Do("SSCAN", key, "0", "COUNT", strconv.Itoa(limit))
		if err != nil {
			return p, err
		}
		if len(v.Array) == 2 {
			p.Items = v.Array[1].Strings()
		}
		if len(p.Items) > limit {
			p.Items = p.Items[:limit]
		}
		sort.Strings(p.Items)
	case "hash":
		if p.Size, err = c.intCmd("HLEN", key); err != nil {
			return p, err
		}
		v, err := c.Do("HSCAN", key, "0", "COUNT", strconv.Itoa(limit))
		if err != nil {
			return p, err
		}
		if len(v.Array) == 2 {
			p.Fields = pairs(v.Array[1].Strings(), limit)
		}
		sort.Slice(p.Fields, func(i, j int) bool { return p.Fields[i].Name < p.Fields[j].Name })
	case "zset":
		if p.Size, err = c.intCmd("ZCARD", key); err != nil {
			return p, err
		}
		v, err := c.Do("ZRANGE", key, "0", lim, "WITHSCORES")
		if err != nil {
			return p, err
		}
		p.Fields = pairs(v.Strings(), limit)
	case "stream":
		if p.Size, err = c.intCmd("XLEN", key); err != nil {
			return p, err
		}
		v, err := c.Do("XRANGE", key, "-", "+", "COUNT", strconv.Itoa(limit))
		if err != nil {
			return p, err
		}
		for _, e := range v.Array {
			if len(e.Array) == 2 {
				p.Items = append(p.Items, e.Array[0].Str+" "+strings.Join(e.Array[1].Strings(), " "))
			}
		}
	}
	if p.Type != "string" {
		shown := len(p.Items) + len(p.Fields)
		p.Truncated = int64(shown) < p.Size
	}
	return p, nil
}

// pairs folds a flat [name, value, name, value…] reply into Fields.
func pairs(flat []string, limit int) []Field {
	out := make([]Field, 0, len(flat)/2)
	for i := 0; i+1 < len(flat) && len(out) < limit; i += 2 {
		out = append(out, Field{Name: flat[i], Value: flat[i+1]})
	}
	return out
}

/* ── MONITOR ────────────────────────────────────────────────── */

// MonitorEvent is one command MONITOR reported.
type MonitorEvent struct {
	// Time is the server's Unix time with microseconds.
	Time float64 `json:"time"`
	DB   int     `json:"db"`
	// Addr is the client ("127.0.0.1:52345", "lua", "unix:/path").
	Addr string   `json:"addr"`
	Args []string `json:"args"`
}

// ParseMonitorLine parses one MONITOR line:
//
//	1700000000.123456 [0 127.0.0.1:52345] "SET" "foo" "bar"
func ParseMonitorLine(line string) (MonitorEvent, error) {
	var ev MonitorEvent
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		return ev, ErrProtocol
	}
	t, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return ev, ErrProtocol
	}
	ev.Time = t
	if !strings.HasPrefix(rest, "[") {
		return ev, ErrProtocol
	}
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return ev, ErrProtocol
	}
	db, addr, _ := strings.Cut(rest[1:end], " ")
	if ev.DB, err = strconv.Atoi(db); err != nil {
		return ev, ErrProtocol
	}
	ev.Addr = addr
	ev.Args, err = parseQuoted(strings.TrimSpace(rest[end+1:]))
	return ev, err
}

// parseQuoted splits MONITOR's argument list: each argument is a
// double-quoted string using C-style escapes (\" \\ \n \r \t \a \b and
// \xHH), as Redis's sdscatrepr writes them.
func parseQuoted(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		if s[i] == ' ' {
			i++
			continue
		}
		if s[i] != '"' {
			return out, ErrProtocol
		}
		i++
		var b strings.Builder
		closed := false
		for i < len(s) {
			ch := s[i]
			if ch == '"' {
				i++
				closed = true
				break
			}
			if ch == '\\' && i+1 < len(s) {
				i++
				switch e := s[i]; e {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case 'a':
					b.WriteByte('\a')
				case 'b':
					b.WriteByte('\b')
				case 'x':
					if i+2 < len(s) {
						if n, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
							b.WriteByte(byte(n))
							i += 2
							break
						}
					}
					b.WriteByte('x')
				default:
					b.WriteByte(e)
				}
				i++
				continue
			}
			b.WriteByte(ch)
			i++
		}
		if !closed {
			return out, ErrProtocol
		}
		out = append(out, b.String())
	}
	return out, nil
}

// Monitor turns the connection into a MONITOR stream and calls fn for
// every command the server reports until fn returns false, ctx ends, or
// the connection fails. The connection is unusable afterwards — close
// it.
func (c *Client) Monitor(ctx context.Context, fn func(MonitorEvent) bool) error {
	if _, err := c.Do("MONITOR"); err != nil {
		return err
	}
	// No per-line deadline: a quiet server sends nothing for a long
	// time. ctx unblocks the read instead.
	c.conn.SetDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { c.conn.SetReadDeadline(time.Now()) })
	defer stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		v, err := readValue(c.r)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if v.Kind != '+' {
			continue
		}
		ev, err := ParseMonitorLine(v.Str)
		if err != nil {
			continue
		}
		if !fn(ev) {
			return nil
		}
	}
}

// FormatArgs renders a command for display: arguments quoted only when
// they need it, long ones shortened.
func FormatArgs(args []string, maxArg int) string {
	parts := make([]string, 0, len(args))
	for i, a := range args {
		if maxArg > 0 && len(a) > maxArg {
			a = a[:maxArg] + "…"
		}
		if i == 0 {
			parts = append(parts, strings.ToUpper(a))
			continue
		}
		if a == "" || strings.ContainsAny(a, " \t\r\n\"") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// ScanAll walks SCAN MATCH pattern to the end (or until max keys),
// returning the keys found and whether it stopped early.
func (c *Client) ScanAll(match string, max int) ([]string, bool, error) {
	var out []string
	seen := map[string]bool{}
	var cursor uint64
	for {
		next, keys, err := c.Scan(cursor, match, 1000, "")
		if err != nil {
			return out, false, err
		}
		for _, k := range keys {
			if !seen[k] { // SCAN may return a key twice
				seen[k] = true
				out = append(out, k)
			}
		}
		if max > 0 && len(out) >= max {
			return out[:max], next != 0 || len(out) > max, nil
		}
		if next == 0 {
			return out, false, nil
		}
		cursor = next
	}
}

// DeleteMatching UNLINKs every key matching pattern (SCAN + UNLINK in
// batches — never KEYS), returning how many were deleted.
func (c *Client) DeleteMatching(pattern string) (int64, error) {
	if pattern == "" || pattern == "*" {
		return 0, fmt.Errorf("refusing to delete every key by pattern — use FLUSHDB")
	}
	var total int64
	var cursor uint64
	for {
		next, keys, err := c.Scan(cursor, pattern, 1000, "")
		if err != nil {
			return total, err
		}
		if len(keys) > 0 {
			n, err := c.Unlink(keys...)
			if err != nil {
				return total, err
			}
			total += n
		}
		if next == 0 {
			return total, nil
		}
		cursor = next
	}
}

// GlobEscape escapes the glob metacharacters SCAN MATCH understands, so
// a literal prefix can be matched as "<escaped>*".
func GlobEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\', '^':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
