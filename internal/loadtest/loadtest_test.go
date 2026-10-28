package loadtest

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestFixedCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		URL:         server.URL,
		Concurrency: 10,
		Requests:    200,
	}

	snap, err := Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if snap.Requests != 200 {
		t.Fatalf("Requests = %d, want 200", snap.Requests)
	}
	if snap.Status["200"] != 200 {
		t.Fatalf("Status[200] = %d, want 200", snap.Status["200"])
	}
	if !(snap.MinMs <= snap.P50Ms && snap.P50Ms <= snap.P95Ms && snap.P95Ms <= snap.P99Ms && snap.P99Ms <= snap.MaxMs) {
		t.Fatalf("percentiles not ordered: min=%v p50=%v p95=%v p99=%v max=%v",
			snap.MinMs, snap.P50Ms, snap.P95Ms, snap.P99Ms, snap.MaxMs)
	}

	sum := 0
	for _, b := range snap.Histogram {
		sum += b.Count
	}
	if sum != 200 {
		t.Fatalf("histogram counts sum to %d, want 200", sum)
	}
}

func TestStatusBreakdown(t *testing.T) {
	var counter int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&counter, 1)
		switch n % 3 {
		case 1:
			w.WriteHeader(http.StatusOK)
		case 2:
			w.WriteHeader(http.StatusNotFound)
		case 0:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	cfg := Config{
		URL:         server.URL,
		Concurrency: 5,
		Requests:    90,
	}

	snap, err := Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if snap.Requests != 90 {
		t.Fatalf("Requests = %d, want 90", snap.Requests)
	}
	if snap.Status["200"] != 30 || snap.Status["404"] != 30 || snap.Status["500"] != 30 {
		t.Fatalf("status breakdown = %+v, want 30/30/30", snap.Status)
	}
	if snap.Errors != 30 {
		t.Fatalf("Errors = %d, want 30 (the 500s)", snap.Errors)
	}
}

func TestDurationRun(t *testing.T) {
	old := minDuration
	minDuration = 50 * time.Millisecond
	defer func() { minDuration = old }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		URL:         server.URL,
		Concurrency: 5,
		Duration:    1 * time.Second,
	}

	snap, err := Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if snap.RPS <= 0 {
		t.Fatalf("RPS = %v, want > 0", snap.RPS)
	}
	if snap.Elapsed < 0.7 || snap.Elapsed > 2.0 {
		t.Fatalf("Elapsed = %v, want ~1s", snap.Elapsed)
	}
	if len(snap.Timeline) == 0 {
		t.Fatalf("Timeline is empty")
	}
	if snap.Phase != "done" {
		t.Fatalf("Phase = %q, want done", snap.Phase)
	}
}

func TestWarmUpExcluded(t *testing.T) {
	var hits int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		URL:         server.URL,
		Concurrency: 5,
		Requests:    20,
		WarmUp:      150 * time.Millisecond,
	}

	snap, err := Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if snap.Requests != 20 {
		t.Fatalf("Requests = %d, want 20", snap.Requests)
	}
	totalHits := atomic.LoadInt64(&hits)
	if totalHits <= 20 {
		t.Fatalf("total server hits = %d, want > 20 (warm-up should add extra, uncounted hits)", totalHits)
	}
}

func TestCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := Config{
		URL:         server.URL,
		Concurrency: 5,
		Duration:    10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	snap, err := Run(ctx, cfg, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if snap.Phase != "stopped" {
		t.Fatalf("Phase = %q, want stopped", snap.Phase)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("Run took %v after cancel, want well under 1.5s", elapsed)
	}
}

func TestTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())

	cfg := Config{
		URL:         server.URL,
		Concurrency: 2,
		Requests:    5,
		RootCAs:     pool,
	}

	snap, err := Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if snap.Status["200"] != 5 {
		t.Fatalf("Status[200] = %d, want 5", snap.Status["200"])
	}
	if snap.Errors != 0 {
		t.Fatalf("Errors = %d, want 0", snap.Errors)
	}

	cfgNoTrust := Config{
		URL:         server.URL,
		Concurrency: 2,
		Requests:    5,
	}
	snap2, err := Run(context.Background(), cfgNoTrust, nil)
	if err != nil {
		t.Fatalf("Run (no trust): %v", err)
	}
	if snap2.Status["error"] == 0 {
		t.Fatalf("expected transport errors without a trusted root, got status=%+v", snap2.Status)
	}
}

func TestValidate(t *testing.T) {
	base := Config{
		URL:         "http://example.test/path",
		Method:      "GET",
		Concurrency: 10,
		Requests:    100,
	}
	if err := Validate(base); err != nil {
		t.Fatalf("base config should be valid: %v", err)
	}

	cases := []struct {
		name string
		mod  func(c Config) Config
	}{
		{"concurrency zero", func(c Config) Config { c.Concurrency = 0; return c }},
		{"concurrency too high", func(c Config) Config { c.Concurrency = 201; return c }},
		{"bad scheme", func(c Config) Config { c.URL = "ftp://example.test/path"; return c }},
		{"no host", func(c Config) Config { c.URL = "http:///path"; return c }},
		{"unparsable URL", func(c Config) Config { c.URL = "http://[::1"; return c }},
		{"bad method token", func(c Config) Config { c.Method = "GE T"; return c }},
		{"warmup negative", func(c Config) Config { c.WarmUp = -1; return c }},
		{"warmup too long", func(c Config) Config { c.WarmUp = 31 * time.Second; return c }},
		{"requests zero", func(c Config) Config { c.Requests = 0; return c }},
		{"requests over max", func(c Config) Config { c.Requests = 1; c.MaxRequests = 10; c.Requests = 11; return c }},
		{"duration too short", func(c Config) Config { c.Requests = 0; c.Duration = 1 * time.Second; return c }},
		{"duration too long", func(c Config) Config { c.Requests = 0; c.Duration = 121 * time.Second; return c }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.mod(base)); err == nil {
				t.Fatalf("expected an error")
			}
		})
	}
}

func TestValidateDurationOK(t *testing.T) {
	c := Config{
		URL:         "https://example.test",
		Concurrency: 1,
		Duration:    5 * time.Second,
	}
	if err := Validate(c); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}
