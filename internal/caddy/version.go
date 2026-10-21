package caddy

import (
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"pm/internal/pmdir"
	"pm/internal/proc"
)

var versionCache struct {
	sync.Mutex
	mod time.Time
	ok  bool
}

var caddyVersionRe = regexp.MustCompile(`v(\d+)\.(\d+)\.`)

// SupportsLogAppend reports whether Mullion's caddy has the log_append
// directive (Caddy 2.8+) — an older binary would reject the whole
// Caddyfile over it. The answer is cached per binary (by its mtime).
func SupportsLogAppend(paths pmdir.Paths) bool {
	fi, err := os.Stat(paths.CaddyExe())
	if err != nil {
		return false
	}
	versionCache.Lock()
	defer versionCache.Unlock()
	if fi.ModTime().Equal(versionCache.mod) {
		return versionCache.ok
	}
	out, err := proc.Quiet(paths.CaddyExe(), "version").Output()
	ok := err == nil && versionAtLeast(string(out), 2, 8)
	versionCache.mod, versionCache.ok = fi.ModTime(), ok
	return ok
}

// versionAtLeast parses `caddy version` output ("v2.11.4 h1:…").
func versionAtLeast(out string, major, minor int) bool {
	m := caddyVersionRe.FindStringSubmatch(out)
	if m == nil {
		return false
	}
	ma, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return ma > major || ma == major && mi >= minor
}
