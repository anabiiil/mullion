// Package loadtest is a small HTTP load-test engine. It drives a fixed
// number of concurrent workers against one URL for a duration or a request
// count, and reports latency percentiles, a status-code breakdown, and a
// per-second timeline — the data behind Mullion's project page "Performance
// → Load test" panel.
package loadtest

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config describes one load-test run.
type Config struct {
	URL                string            // absolute http(s) URL
	Method             string            // default GET
	Headers            map[string]string // e.g. "Cookie": "...", "Accept": "..."
	Body               string            // optional request body
	Concurrency        int               // 1..200 (Validate rejects outside)
	Duration           time.Duration     // run length (5s..120s when Requests==0); 0 = run by Requests
	Requests           int               // total requests (when Duration==0); capped by MaxRequests
	WarmUp             time.Duration     // optional: requests during warm-up are sent but NOT counted (0..30s)
	MaxRequests        int               // hard cap on counted requests (default 200000 when 0)
	Timeout            time.Duration     // per request, default 30s
	RootCAs            *x509.CertPool    // extra trusted roots (Caddy's local CA); nil = system
	InsecureSkipVerify bool              // caller decides when that's allowed

	// Activity, when set, is called at most once per second while the test
	// runs (the caller uses it to keep a sleeping dev server awake).
	Activity func()
}

// Bucket is one histogram bin. LeMs is the bin's upper bound in
// milliseconds; a request with latency <= LeMs (and > the previous bin's
// bound) falls into it. The last bin has no upper bound — it is encoded as
// LeMs == -1 (read it as +Inf) and holds every latency past the highest
// finite bound.
type Bucket struct {
	LeMs  float64 `json:"le"`
	Count int     `json:"count"`
}

// Point is one second of the measured run.
type Point struct {
	T      float64 `json:"t"`
	RPS    float64 `json:"rps"`
	P95    float64 `json:"p95"`
	Errors int     `json:"errors"`
}

// Snapshot is a point-in-time (or final) view of a run's results.
type Snapshot struct {
	Elapsed  float64 `json:"elapsed"` // seconds since measurement started (after warm-up)
	Phase    string  `json:"phase"`   // "warmup" | "running" | "done" | "stopped"
	Done     bool    `json:"done"`
	Requests int     `json:"requests"` // completed counted requests (including errors)
	Errors   int     `json:"errors"`   // transport errors + status >= 500
	RPS      float64 `json:"rps"`

	MinMs  float64 `json:"min"`
	MaxMs  float64 `json:"max"`
	MeanMs float64 `json:"mean"`
	P50Ms  float64 `json:"p50"`
	P90Ms  float64 `json:"p90"`
	P95Ms  float64 `json:"p95"`
	P99Ms  float64 `json:"p99"`

	Status       map[string]int `json:"status"`       // "200": n, "404": n, "error": n (transport errors)
	ErrorSamples []string       `json:"errorSamples"` // up to 5 distinct error messages
	Histogram    []Bucket       `json:"histogram"`
	Timeline     []Point        `json:"timeline"`
	BytesIn      int64          `json:"bytesIn"`
}

// defaultMaxRequests is the hard cap on counted requests when
// Config.MaxRequests is left at zero.
const defaultMaxRequests = 200000

// defaultTimeout is the per-request timeout when Config.Timeout is zero.
const defaultTimeout = 30 * time.Second

// minDuration and maxDuration bound Config.Duration. minDuration is a
// package var, not a const, so tests can shrink it and run a sub-5s
// duration-based test without waiting out the real minimum.
var minDuration = 5 * time.Second

const maxDuration = 120 * time.Second

// maxWarmUp bounds Config.WarmUp.
const maxWarmUp = 30 * time.Second

// Validate checks c for a runnable configuration without starting anything.
func Validate(c Config) error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL must include a host")
	}

	method := c.Method
	if method == "" {
		method = "GET"
	}
	if !isValidMethodToken(method) {
		return fmt.Errorf("invalid HTTP method %q", method)
	}

	if c.Concurrency < 1 || c.Concurrency > 200 {
		return fmt.Errorf("concurrency must be between 1 and 200, got %d", c.Concurrency)
	}

	if c.WarmUp < 0 || c.WarmUp > maxWarmUp {
		return fmt.Errorf("warm-up must be between 0 and %s, got %s", maxWarmUp, c.WarmUp)
	}

	if c.Duration == 0 {
		maxRequests := c.MaxRequests
		if maxRequests <= 0 {
			maxRequests = defaultMaxRequests
		}
		if c.Requests < 1 || c.Requests > maxRequests {
			return fmt.Errorf("requests must be between 1 and %d, got %d", maxRequests, c.Requests)
		}
	} else if c.Duration < minDuration || c.Duration > maxDuration {
		return fmt.Errorf("duration must be between %s and %s, got %s", minDuration, maxDuration, c.Duration)
	}

	return nil
}

// isValidMethodToken reports whether s is a valid HTTP method token (RFC
// 7230 §3.2.6): one or more tchar characters.
func isValidMethodToken(s string) bool {
	if s == "" {
		return false
	}
	const extra = "!#$%&'*+-.^_`|~"
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune(extra, r):
		default:
			return false
		}
	}
	return true
}
