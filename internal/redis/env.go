package redis

// Reading a project's .env to find which Redis keys are its own, and
// planning the .env edits of "Use Redis in this project".
//
// Laravel's key layout (checked against laravel/laravel and
// laravel/framework 10.x–12.x, config/database.php and config/cache.php):
//
//   - every key on every Redis connection starts with the connection
//     prefix, env REDIS_PREFIX, defaulting to Str::slug(APP_NAME, '_') .
//     '_database_' (framework config, and the 10.x/11.x skeleton) or
//     Str::slug(APP_NAME) . '-database-' (the 12.x skeleton's published
//     config);
//   - cache entries add the cache prefix after it: env CACHE_PREFIX,
//     default Str::slug(APP_NAME, '_') . '_cache_' (or '-cache-' in the
//     12.x skeleton), on the "cache" connection — database REDIS_CACHE_DB
//     (default 1);
//   - sessions, queues and everything else live on the "default"
//     connection — database REDIS_DB (default 0).
//
// Which default applies is read from the project's own config file when
// it has one, else from vendor/laravel/framework's.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ParseEnv reads a .env file's assignments (last one wins), unquoting
// values the way Laravel's dotenv does for the common cases: "double"
// (with \n and \" escapes), 'single', and bare values with a trailing
// " # comment" removed. ${VAR} references to earlier keys are expanded.
func ParseEnv(text string) map[string]string {
	vals := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, raw, ok := envAssignment(line)
		if !ok {
			continue
		}
		vals[key] = unquoteEnv(raw, vals)
	}
	return vals
}

// envAssignment splits one line into key and raw value.
func envAssignment(line string) (string, string, bool) {
	l := strings.TrimSpace(strings.TrimRight(line, "\r"))
	if l == "" || strings.HasPrefix(l, "#") {
		return "", "", false
	}
	l = strings.TrimPrefix(l, "export ")
	key, val, ok := strings.Cut(l, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return "", "", false
	}
	return key, strings.TrimSpace(val), true
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func unquoteEnv(raw string, vals map[string]string) string {
	switch {
	case len(raw) >= 2 && raw[0] == '\'':
		if end := strings.IndexByte(raw[1:], '\''); end >= 0 {
			return raw[1 : end+1]
		}
	case len(raw) >= 2 && raw[0] == '"':
		var b strings.Builder
		for i := 1; i < len(raw); i++ {
			ch := raw[i]
			if ch == '\\' && i+1 < len(raw) {
				i++
				switch raw[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(raw[i])
				}
				continue
			}
			if ch == '"' {
				break
			}
			b.WriteByte(ch)
		}
		return expandEnv(b.String(), vals)
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return expandEnv(raw, vals)
}

func expandEnv(s string, vals map[string]string) string {
	return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		return vals[m[2:len(m)-1]]
	})
}

// envValue is what Laravel's env() returns for a key: (value, set).
// "null"/"(null)" read as unset-to-null, "empty"/"(empty)" as "".
func envValue(vals map[string]string, key string) (string, bool, bool) {
	v, ok := vals[key]
	if !ok {
		return "", false, false
	}
	switch strings.ToLower(v) {
	case "null", "(null)":
		return "", true, true
	case "empty", "(empty)":
		return "", true, false
	}
	return v, true, false
}

// Slug mimics Laravel's Str::slug(title, sep) for the names projects
// use (Str::slug also transliterates to ASCII first; letters outside
// ASCII are kept here as they are).
func Slug(title, sep string) string {
	flip := "_"
	if sep == "_" {
		flip = "-"
	}
	s := strings.ReplaceAll(title, flip, sep)
	s = strings.ReplaceAll(s, "@", sep+"at"+sep)
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if string(r) == sep || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	// Runs of separators and whitespace collapse into one separator.
	var out strings.Builder
	pending := false
	for _, r := range b.String() {
		if string(r) == sep || unicode.IsSpace(r) {
			pending = true
			continue
		}
		if pending && out.Len() > 0 {
			out.WriteString(sep)
		}
		pending = false
		out.WriteRune(r)
	}
	return out.String()
}

// prefixFormula is how a config file builds a default prefix.
type prefixFormula struct {
	sep     string // Str::slug separator
	suffix  string // literal appended after the slug
	literal string // a plain string default (no slug)
	isLit   bool
}

var (
	slugDefaultRe = regexp.MustCompile(`Str::slug\(\s*(?:\(string\)\s*)?env\(\s*['"]APP_NAME['"]\s*,\s*['"][^'"]*['"]\s*\)\s*(?:,\s*['"]([-_])['"]\s*)?\)\s*\.\s*['"]([^'"]*)['"]`)
	literalRe     = regexp.MustCompile(`^\s*['"]([^'"]*)['"]\s*\)`)
)

// readFormula finds env('KEY', <default>) in a config file.
func readFormula(path, key string) (prefixFormula, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return prefixFormula{}, false
	}
	text := string(data)
	i := strings.Index(text, "env('"+key+"'")
	if i < 0 {
		i = strings.Index(text, `env("`+key+`"`)
	}
	if i < 0 {
		return prefixFormula{}, false
	}
	rest := text[i+len(key)+6:]
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimLeft(rest, " ,")
	if m := slugDefaultRe.FindStringSubmatch(rest); m != nil {
		sep := m[1]
		if sep == "" {
			sep = "-"
		}
		return prefixFormula{sep: sep, suffix: m[2]}, true
	}
	if m := literalRe.FindStringSubmatch(rest); m != nil {
		return prefixFormula{literal: m[1], isLit: true}, true
	}
	return prefixFormula{}, false
}

// formulaFor resolves the default for a prefix key from the project's
// config, then the framework's, then Laravel's framework default.
func formulaFor(dir, file, key string, fallback prefixFormula) prefixFormula {
	for _, p := range []string{
		filepath.Join(dir, "config", file),
		filepath.Join(dir, "vendor", "laravel", "framework", "config", file),
	} {
		if f, ok := readFormula(p, key); ok {
			return f
		}
	}
	return fallback
}

func (f prefixFormula) apply(appName string) string {
	if f.isLit {
		return f.literal
	}
	return Slug(appName, f.sep) + f.suffix
}

// ProjectRedis is how a project talks to Redis, as far as its .env (and
// Laravel's defaults) tell.
type ProjectRedis struct {
	Laravel bool   `json:"laravel"`
	HasEnv  bool   `json:"hasEnv"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	// DB is the default connection's database (sessions, queues);
	// CacheDB the cache connection's.
	DB      int `json:"db"`
	CacheDB int `json:"cacheDb"`
	// Prefix is the connection prefix every key starts with;
	// CachePrefix what cache entries add after it.
	Prefix      string `json:"prefix"`
	CachePrefix string `json:"cachePrefix"`
	Client      string `json:"client"`
	// CacheStore / SessionDriver / QueueConnection are the drivers the
	// .env selects (CACHE_STORE, or CACHE_DRIVER on Laravel 10 and
	// older).
	CacheStore      string `json:"cacheStore"`
	SessionDriver   string `json:"sessionDriver"`
	QueueConnection string `json:"queueConnection"`
	// LegacyCacheKey: the project selects its cache store with
	// CACHE_DRIVER (Laravel ≤ 10) rather than CACHE_STORE.
	LegacyCacheKey bool `json:"legacyCacheKey"`
	// UsesRedis: something (cache, session or queue) is on Redis.
	UsesRedis bool `json:"usesRedis"`
	// PointsHere: REDIS_HOST/PORT address Mullion's Redis.
	PointsHere bool `json:"pointsHere"`
}

// ProjectSettings reads dir/.env (missing = Laravel's defaults) for the
// project's Redis settings. mullionPort is where Mullion's Redis
// listens, for PointsHere.
func ProjectSettings(dir string, mullionPort int) ProjectRedis {
	p := ProjectRedis{Host: "127.0.0.1", Port: DefaultPort, DB: 0, CacheDB: 1, Client: "phpredis"}
	_, err := os.Stat(filepath.Join(dir, "artisan"))
	p.Laravel = err == nil
	data, err := os.ReadFile(filepath.Join(dir, ".env"))
	p.HasEnv = err == nil
	vals := ParseEnv(string(data))

	if v, ok, _ := envValue(vals, "REDIS_HOST"); ok && v != "" {
		p.Host = v
	}
	if v, ok, _ := envValue(vals, "REDIS_PORT"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.Port = n
		}
	}
	if v, ok, _ := envValue(vals, "REDIS_DB"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.DB = n
		}
	}
	if v, ok, _ := envValue(vals, "REDIS_CACHE_DB"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.CacheDB = n
		}
	}
	if v, ok, _ := envValue(vals, "REDIS_CLIENT"); ok && v != "" {
		p.Client = v
	}
	appName := "laravel"
	if v, ok, isNull := envValue(vals, "APP_NAME"); ok && !isNull {
		appName = v
	}
	p.Prefix = resolvePrefix(dir, vals, "REDIS_PREFIX", "database.php",
		prefixFormula{sep: "_", suffix: "_database_"}, appName)
	p.CachePrefix = resolvePrefix(dir, vals, "CACHE_PREFIX", "cache.php",
		prefixFormula{sep: "_", suffix: "_cache_"}, appName)

	if v, ok := vals["CACHE_STORE"]; ok {
		p.CacheStore = v
	} else if v, ok := vals["CACHE_DRIVER"]; ok {
		p.CacheStore, p.LegacyCacheKey = v, true
	}
	p.SessionDriver = vals["SESSION_DRIVER"]
	p.QueueConnection = vals["QUEUE_CONNECTION"]
	p.UsesRedis = p.CacheStore == "redis" || p.SessionDriver == "redis" || p.QueueConnection == "redis"
	loop := p.Host == "127.0.0.1" || p.Host == "localhost" || p.Host == "::1"
	p.PointsHere = loop && p.Port == mullionPort
	return p
}

func resolvePrefix(dir string, vals map[string]string, key, file string, fallback prefixFormula, appName string) string {
	if v, ok, _ := envValue(vals, key); ok {
		return v
	}
	return formulaFor(dir, file, key, fallback).apply(appName)
}

/* ── "Use Redis in this project": the .env edit plan ────────── */

// EnvChange is one key the plan sets.
type EnvChange struct {
	Key string `json:"key"`
	// Old is the current raw value ("" when Exists is false).
	Old    string `json:"old"`
	New    string `json:"new"`
	Exists bool   `json:"exists"`
}

// LaravelEnv is what "Use Redis in this project" writes, in order.
// legacy selects CACHE_DRIVER (Laravel ≤ 10) instead of CACHE_STORE.
func LaravelEnv(port int, password string, legacy bool) [][2]string {
	pw := "null"
	if password != "" {
		pw = password
	}
	cacheKey := "CACHE_STORE"
	if legacy {
		cacheKey = "CACHE_DRIVER"
	}
	return [][2]string{
		{"REDIS_CLIENT", "phpredis"},
		{"REDIS_HOST", "127.0.0.1"},
		{"REDIS_PASSWORD", pw},
		{"REDIS_PORT", strconv.Itoa(port)},
		{cacheKey, "redis"},
		{"SESSION_DRIVER", "redis"},
		{"QUEUE_CONNECTION", "redis"},
	}
}

// PlanEnv lists the changes that bring text's assignments to want —
// keys already at the wanted value are left out.
func PlanEnv(text string, want [][2]string) []EnvChange {
	cur := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, raw, ok := envAssignment(line); ok {
			cur[k] = raw
		}
	}
	vals := ParseEnv(text)
	var out []EnvChange
	for _, kv := range want {
		raw, exists := cur[kv[0]]
		if exists && vals[kv[0]] == kv[1] {
			continue
		}
		out = append(out, EnvChange{Key: kv[0], Old: raw, New: kv[1], Exists: exists})
	}
	return out
}

// ApplyEnv rewrites text with the changes: an existing key's last
// assignment is replaced in place (earlier duplicates are left alone —
// the last one wins anyway), new keys are appended under a comment.
// Values that need it are double-quoted.
func ApplyEnv(text string, changes []EnvChange) string {
	if len(changes) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	last := map[string]int{}
	for i, line := range lines {
		if k, _, ok := envAssignment(line); ok {
			last[k] = i
		}
	}
	var added []string
	for _, c := range changes {
		line := c.Key + "=" + quoteEnv(c.New)
		if i, ok := last[c.Key]; ok {
			prefix := ""
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "export ") {
				prefix = "export "
			}
			lines[i] = prefix + line
			continue
		}
		added = append(added, line)
	}
	out := strings.Join(lines, "\n")
	if len(added) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out != "" && !strings.HasSuffix(out, "\n\n") {
			out += "\n"
		}
		out += "# Redis (added by Mullion)\n" + strings.Join(added, "\n") + "\n"
	}
	return out
}

func quoteEnv(v string) string {
	if v == "" || strings.ContainsAny(v, " \t#\"'$\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return v
}

// DiffEnv renders the plan as a unified-style diff for the confirm
// dialog.
func DiffEnv(changes []EnvChange) string {
	var b strings.Builder
	for _, c := range changes {
		if c.Exists {
			b.WriteString("- " + c.Key + "=" + c.Old + "\n")
		}
		b.WriteString("+ " + c.Key + "=" + quoteEnv(c.New) + "\n")
	}
	return b.String()
}

// SortedKeys is a map's keys, sorted (for stable output).
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
