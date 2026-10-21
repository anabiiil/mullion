package phpver

// OPcache settings per PHP version: read and written in the version's
// php.ini, plus its live status read from the running php-fpm/php-cgi
// (fcgiclient.go) — `php -r 'opcache_get_status()'` would only describe
// the CLI's own (normally disabled) cache, never the one serving sites.
//
// Platforms:
//   - macOS/Linux (static-php.dev builds): OPcache is compiled in, so
//     only the opcache.* directives matter. A `zend_extension=opcache`
//     line must NOT be there — PHP then looks for an opcache.so that
//     doesn't exist and warns on every run — and one found is commented
//     out.
//   - Windows (windows.php.net builds): up to PHP 8.4 OPcache is
//     ext\php_opcache.dll, loaded with `zend_extension=opcache`; that
//     line is kept active while OPcache is on for sites or the CLI, and
//     commented out when both are off. PHP 8.5 made OPcache a mandatory,
//     built-in part of PHP (no DLL ships), so there only the directives
//     apply — the same as macOS.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// JIT modes the panel offers, and the opcache.jit value each writes.
// "tracing" and "function" are PHP's aliases for 1254 and 1205.
var jitValues = map[string]string{
	"off":      "disable",
	"tracing":  "tracing",
	"function": "function",
}

// Opcache is one version's OPcache configuration, as php.ini sets it
// (PHP's defaults where it doesn't).
type Opcache struct {
	Enabled bool `json:"enabled"` // opcache.enable (php-fpm/php-cgi — sites)
	CLI     bool `json:"cli"`     // opcache.enable_cli
	// JIT is off|tracing|function, or "custom" for any other
	// opcache.jit value (JITRaw holds it).
	JIT                string `json:"jit"`
	JITRaw             string `json:"jitRaw"`
	JITBuffer          string `json:"jitBuffer"`  // opcache.jit_buffer_size
	Memory             int    `json:"memory"`     // opcache.memory_consumption, MB
	MaxFiles           int    `json:"maxFiles"`   // opcache.max_accelerated_files
	ValidateTimestamps bool   `json:"validate"`   // opcache.validate_timestamps
	RevalidateFreq     int    `json:"revalidate"` // opcache.revalidate_freq, seconds
	Builtin            bool   `json:"builtin"`    // compiled in (no zend_extension line needed)
	ExtLine            bool   `json:"extLine"`    // Windows: zend_extension=opcache is active
	Platform           string `json:"platform"`   // runtime.GOOS
	ConfigNote         string `json:"configNote"` // platform remark for the panel
	IniPath            string `json:"iniPath"`
}

// opcacheGOOS is the platform whose rules apply — a seam for tests.
var opcacheGOOS = runtime.GOOS

// opcacheDefaults are PHP's own defaults for a version: JIT became
// opcache.jit=disable + a 64M buffer in 8.4; before, jit=tracing with a
// 0 buffer (so effectively off).
func opcacheDefaults(version string) Opcache {
	o := Opcache{Enabled: true, Memory: 128, MaxFiles: 10000, ValidateTimestamps: true, RevalidateFreq: 2,
		JIT: "off", JITRaw: "disable", JITBuffer: "64M"}
	if sel, err := ParseSelector(version); err == nil && (sel.Major < 8 || sel.Major == 8 && sel.Minor < 4) {
		o.JITRaw, o.JITBuffer = "tracing", "0"
	}
	return o
}

// jitMode maps an opcache.jit value (and buffer) to off|tracing|function|custom.
func jitMode(raw, buffer string) string {
	r := strings.ToLower(strings.Trim(strings.TrimSpace(raw), `"'`))
	if sizeBytes(buffer) == 0 {
		return "off" // no buffer: PHP doesn't JIT whatever opcache.jit says
	}
	switch r {
	case "disable", "off", "0", "":
		return "off"
	case "tracing", "on", "1254":
		return "tracing"
	case "function", "1205":
		return "function"
	}
	return "custom"
}

// sizeBytes parses an ini size ("64M", "0", "128k").
func sizeBytes(v string) int64 {
	v = strings.TrimSpace(strings.Trim(v, `"'`))
	if v == "" {
		return 0
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'k', 'K':
		mult, v = 1<<10, v[:len(v)-1]
	case 'm', 'M':
		mult, v = 1<<20, v[:len(v)-1]
	case 'g', 'G':
		mult, v = 1<<30, v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

// opcacheDLL reports whether this build ships OPcache as a DLL that
// must be loaded (Windows, PHP < 8.5).
func opcacheDLL(dir string) bool {
	if opcacheGOOS != "windows" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "ext", "php_opcache.dll"))
	return err == nil
}

// zendOpcacheActive reports whether an ini has an active
// zend_extension=opcache line.
func zendOpcacheActive(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if n, active, zend := parseExtLine(line); n == "opcache" && active && zend {
			return true
		}
	}
	return false
}

// ReadOpcache reads a version's OPcache configuration from its php.ini.
func ReadOpcache(paths pmdir.Paths, version string) (Opcache, error) {
	dir := paths.PhpVersionDir(version)
	ini := filepath.Join(dir, "php.ini")
	data, err := os.ReadFile(ini)
	if err != nil {
		return Opcache{}, fmt.Errorf("reading %s: %w", ini, err)
	}
	return parseOpcache(string(data), version, dir, ini), nil
}

func parseOpcache(text, version, dir, ini string) Opcache {
	o := opcacheDefaults(version)
	o.Platform, o.IniPath = opcacheGOOS, ini
	vals := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		eq := strings.Index(l, "=")
		if l == "" || strings.HasPrefix(l, ";") || strings.HasPrefix(l, "[") || eq < 0 {
			continue
		}
		v := strings.TrimSpace(l[eq+1:])
		if i := strings.Index(v, " ;"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		vals[strings.TrimSpace(l[:eq])] = strings.Trim(v, `"'`)
	}
	boolOf := func(k string, def bool) bool {
		v, ok := vals[k]
		if !ok {
			return def
		}
		return iniBoolValue(v) == "1"
	}
	intOf := func(k string, def int) int {
		if n, err := strconv.Atoi(vals[k]); err == nil {
			return n
		}
		return def
	}
	o.Enabled = boolOf("opcache.enable", o.Enabled)
	o.CLI = boolOf("opcache.enable_cli", o.CLI)
	o.Memory = intOf("opcache.memory_consumption", o.Memory)
	o.MaxFiles = intOf("opcache.max_accelerated_files", o.MaxFiles)
	o.ValidateTimestamps = boolOf("opcache.validate_timestamps", o.ValidateTimestamps)
	o.RevalidateFreq = intOf("opcache.revalidate_freq", o.RevalidateFreq)
	if v, ok := vals["opcache.jit"]; ok {
		o.JITRaw = v
	}
	if v, ok := vals["opcache.jit_buffer_size"]; ok {
		o.JITBuffer = v
	}
	o.JIT = jitMode(o.JITRaw, o.JITBuffer)
	dll := opcacheDLL(dir)
	o.Builtin = !dll
	o.ExtLine = zendOpcacheActive(text)
	if dll && !o.ExtLine {
		// Without the DLL loaded there is no OPcache at all.
		o.Enabled, o.CLI = false, false
	}
	switch {
	case opcacheGOOS != "windows":
		o.ConfigNote = "OPcache is compiled into this PHP build — only the opcache.* settings apply"
	case dll:
		o.ConfigNote = "OPcache is ext\\php_opcache.dll here, loaded by zend_extension=opcache while it's on"
	default:
		o.ConfigNote = "OPcache is built into PHP 8.5+ — only the opcache.* settings apply"
	}
	return o
}

// OpcacheChange is a partial update: nil fields are left as they are.
type OpcacheChange struct {
	Enabled            *bool   `json:"enabled,omitempty"`
	CLI                *bool   `json:"cli,omitempty"`
	JIT                *string `json:"jit,omitempty"`
	JITBuffer          *string `json:"jitBuffer,omitempty"`
	Memory             *int    `json:"memory,omitempty"`
	MaxFiles           *int    `json:"maxFiles,omitempty"`
	ValidateTimestamps *bool   `json:"validate,omitempty"`
	RevalidateFreq     *int    `json:"revalidate,omitempty"`
}

// Empty reports whether the change sets nothing.
func (c OpcacheChange) Empty() bool {
	return c.Enabled == nil && c.CLI == nil && c.JIT == nil && c.JITBuffer == nil && c.Memory == nil &&
		c.MaxFiles == nil && c.ValidateTimestamps == nil && c.RevalidateFreq == nil
}

func onOff(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// validate checks the change's values.
func (c OpcacheChange) validate() error {
	if c.JIT != nil {
		if _, ok := jitValues[*c.JIT]; !ok {
			return fmt.Errorf("JIT mode %q: use off, tracing or function", *c.JIT)
		}
	}
	if c.JITBuffer != nil && !iniSizeRe.MatchString(*c.JITBuffer) || c.JITBuffer != nil && *c.JITBuffer == "-1" {
		return fmt.Errorf("JIT buffer size %q: use a size like 64M (0 = none)", *c.JITBuffer)
	}
	if c.Memory != nil && (*c.Memory < 8 || *c.Memory > 4096) {
		return fmt.Errorf("memory %d MB: use 8-4096", *c.Memory)
	}
	if c.MaxFiles != nil && (*c.MaxFiles < 200 || *c.MaxFiles > 1000000) {
		return fmt.Errorf("max accelerated files %d: use 200-1000000", *c.MaxFiles)
	}
	if c.RevalidateFreq != nil && (*c.RevalidateFreq < 0 || *c.RevalidateFreq > 86400) {
		return fmt.Errorf("revalidate frequency %d s: use 0-86400", *c.RevalidateFreq)
	}
	return nil
}

// SetOpcache writes a change into a version's php.ini (restarting
// php-fpm/php-cgi is the caller's job). Turning JIT on with no buffer
// (PHP < 8.4's default 0) also gives it a 64M buffer — without one PHP
// silently doesn't JIT.
func SetOpcache(paths pmdir.Paths, version string, c OpcacheChange) error {
	if err := c.validate(); err != nil {
		return err
	}
	dir := paths.PhpVersionDir(version)
	ini := filepath.Join(dir, "php.ini")
	cur, err := ReadOpcache(paths, version)
	if err != nil {
		return err
	}
	set := func(k, v string) error {
		if err := setIniDirective(ini, k, v); err != nil {
			return fmt.Errorf("setting %s in %s: %w", k, ini, err)
		}
		return nil
	}
	if c.Enabled != nil {
		if err := set("opcache.enable", onOff(*c.Enabled)); err != nil {
			return err
		}
		cur.Enabled = *c.Enabled
	}
	if c.CLI != nil {
		if err := set("opcache.enable_cli", onOff(*c.CLI)); err != nil {
			return err
		}
		cur.CLI = *c.CLI
	}
	if c.JIT != nil {
		if err := set("opcache.jit", jitValues[*c.JIT]); err != nil {
			return err
		}
		if *c.JIT != "off" && c.JITBuffer == nil && sizeBytes(cur.JITBuffer) == 0 {
			b := "64M"
			c.JITBuffer = &b
		}
	}
	if c.JITBuffer != nil {
		if err := set("opcache.jit_buffer_size", *c.JITBuffer); err != nil {
			return err
		}
	}
	if c.Memory != nil {
		if err := set("opcache.memory_consumption", strconv.Itoa(*c.Memory)); err != nil {
			return err
		}
	}
	if c.MaxFiles != nil {
		if err := set("opcache.max_accelerated_files", strconv.Itoa(*c.MaxFiles)); err != nil {
			return err
		}
	}
	if c.ValidateTimestamps != nil {
		if err := set("opcache.validate_timestamps", onOff(*c.ValidateTimestamps)); err != nil {
			return err
		}
	}
	if c.RevalidateFreq != nil {
		if err := set("opcache.revalidate_freq", strconv.Itoa(*c.RevalidateFreq)); err != nil {
			return err
		}
	}
	return syncZendOpcacheLine(ini, dir, cur.Enabled || cur.CLI)
}

// syncZendOpcacheLine keeps the zend_extension=opcache line right for
// the platform: on Windows with php_opcache.dll, active while OPcache
// is wanted (want) and commented out otherwise; everywhere else (built
// in) never active.
func syncZendOpcacheLine(ini, dir string, want bool) error {
	data, err := os.ReadFile(ini)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(ini); err == nil {
		mode = st.Mode()
	}
	need := want && opcacheDLL(dir)
	lines := strings.Split(string(data), "\n")
	changed, satisfied := false, false
	for i, line := range lines {
		n, active, zend := parseExtLine(line)
		if n != "opcache" || !zend {
			continue
		}
		switch {
		case need && active:
			satisfied = true
		case need && !active && !satisfied:
			lines[i] = "zend_extension=opcache"
			satisfied, changed = true, true
		case !need && active:
			lines[i] = ";" + strings.TrimRight(line, "\r")
			changed = true
		}
	}
	if need && !satisfied {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "zend_extension=opcache", "")
		changed = true
	}
	if !changed {
		return nil
	}
	return os.WriteFile(ini, []byte(strings.Join(lines, "\n")), mode)
}

// DevOpcacheIni is what a fresh install's php.ini gets: OPcache on for
// sites (off for the CLI — PHP's default), JIT off, and timestamps
// checked on every request (revalidate_freq=0) so an edited file shows
// on the next reload instead of up to 2 seconds later. Windows adds
// zend_extension=opcache itself when the build has the DLL.
func DevOpcacheIni() string {
	return strings.Join([]string{
		"opcache.enable=1",
		"opcache.enable_cli=0",
		"opcache.validate_timestamps=1",
		"opcache.revalidate_freq=0",
		"opcache.jit=disable",
	}, "\n") + "\n"
}

/* ── live status ───────────────────────────────────────────── */

// OpcacheStatus is opcache_get_status() as the version's php-fpm (or
// php-cgi on Windows) — the one serving sites — reports it.
type OpcacheStatus struct {
	PHP           string  `json:"php"`
	SAPI          string  `json:"sapi"`
	Loaded        bool    `json:"loaded"`  // the extension is there at all
	Enabled       bool    `json:"enabled"` // opcache_enabled
	CacheFull     bool    `json:"cacheFull"`
	Hits          int64   `json:"hits"`
	Misses        int64   `json:"misses"`
	HitRate       float64 `json:"hitRate"`
	CachedScripts int64   `json:"cachedScripts"`
	CachedKeys    int64   `json:"cachedKeys"`
	MaxKeys       int64   `json:"maxKeys"`
	MemoryUsed    int64   `json:"memoryUsed"`
	MemoryFree    int64   `json:"memoryFree"`
	MemoryWasted  int64   `json:"memoryWasted"`
	StartTime     int64   `json:"startTime"`
	OOMRestarts   int64   `json:"oomRestarts"`
	JITEnabled    bool    `json:"jitEnabled"`
	JITOn         bool    `json:"jitOn"`
	JITBuffer     int64   `json:"jitBuffer"`
	JITFree       int64   `json:"jitFree"`
}

// statusPHP prints the status as JSON; opcache_get_status(false) leaves
// out the per-script list.
const statusPHP = `<?php
// Written by mullion to read OPcache's status through php-fpm/php-cgi.
header('Content-Type: application/json');
$out = array('php' => PHP_VERSION, 'sapi' => PHP_SAPI, 'loaded' => function_exists('opcache_get_status'));
if ($out['loaded']) { $out['status'] = @opcache_get_status(false); }
echo json_encode($out);
`

// parseOpcacheStatus maps the status script's JSON.
func parseOpcacheStatus(body []byte) (OpcacheStatus, error) {
	var raw struct {
		PHP    string          `json:"php"`
		SAPI   string          `json:"sapi"`
		Loaded bool            `json:"loaded"`
		Status json.RawMessage `json:"status"` // false when OPcache is off
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		text := strings.TrimSpace(string(body))
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		return OpcacheStatus{}, fmt.Errorf("unexpected answer from PHP: %s", text)
	}
	s := OpcacheStatus{PHP: raw.PHP, SAPI: raw.SAPI, Loaded: raw.Loaded}
	if len(raw.Status) == 0 || raw.Status[0] != '{' {
		return s, nil
	}
	var st struct {
		Enabled bool `json:"opcache_enabled"`
		Full    bool `json:"cache_full"`
		Memory  struct {
			Used   int64 `json:"used_memory"`
			Free   int64 `json:"free_memory"`
			Wasted int64 `json:"wasted_memory"`
		} `json:"memory_usage"`
		Stats struct {
			Scripts     int64   `json:"num_cached_scripts"`
			Keys        int64   `json:"num_cached_keys"`
			MaxKeys     int64   `json:"max_cached_keys"`
			Hits        int64   `json:"hits"`
			Misses      int64   `json:"misses"`
			HitRate     float64 `json:"opcache_hit_rate"`
			StartTime   int64   `json:"start_time"`
			OOMRestarts int64   `json:"oom_restarts"`
		} `json:"opcache_statistics"`
		JIT *struct {
			Enabled bool  `json:"enabled"`
			On      bool  `json:"on"`
			Buffer  int64 `json:"buffer_size"`
			Free    int64 `json:"buffer_free"`
		} `json:"jit"`
	}
	if err := json.Unmarshal(raw.Status, &st); err != nil {
		return s, fmt.Errorf("reading OPcache's status: %w", err)
	}
	s.Enabled, s.CacheFull = st.Enabled, st.Full
	s.MemoryUsed, s.MemoryFree, s.MemoryWasted = st.Memory.Used, st.Memory.Free, st.Memory.Wasted
	s.CachedScripts, s.CachedKeys, s.MaxKeys = st.Stats.Scripts, st.Stats.Keys, st.Stats.MaxKeys
	s.Hits, s.Misses, s.HitRate = st.Stats.Hits, st.Stats.Misses, st.Stats.HitRate
	s.StartTime, s.OOMRestarts = st.Stats.StartTime, st.Stats.OOMRestarts
	if st.JIT != nil {
		s.JITEnabled, s.JITOn, s.JITBuffer, s.JITFree = st.JIT.Enabled, st.JIT.On, st.JIT.Buffer, st.JIT.Free
	}
	return s, nil
}

// LiveOpcacheStatus asks the version's running php-fpm/php-cgi for its
// OPcache status over FastCGI (straight to its loopback port, the way
// Caddy talks to it). The script lives in Mullion's tmp folder, outside
// every site's root, so no browser can reach it.
func LiveOpcacheStatus(paths pmdir.Paths, version string) (OpcacheStatus, error) {
	port, err := FcgiPort(version)
	if err != nil {
		return OpcacheStatus{}, err
	}
	if err := os.MkdirAll(paths.TmpDir(), 0o755); err != nil {
		return OpcacheStatus{}, err
	}
	// Written only when missing or changed: OPcache skips files modified
	// in the last 2 seconds (opcache.file_update_protection), so a fresh
	// copy every poll would count a miss each time; an unchanged one is
	// cached once and adds a single hit per poll.
	script := filepath.Join(paths.TmpDir(), "mullion-opcache-status.php")
	if cur, err := os.ReadFile(script); err != nil || string(cur) != statusPHP {
		if err := os.WriteFile(script, []byte(statusPHP), 0o644); err != nil {
			return OpcacheStatus{}, err
		}
	}
	body, err := fcgiGet("127.0.0.1:"+strconv.Itoa(port), script, 5*time.Second)
	if err != nil {
		return OpcacheStatus{}, err
	}
	return parseOpcacheStatus(body)
}
