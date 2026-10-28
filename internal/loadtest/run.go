package loadtest

import (
	"context"
	"crypto/tls"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// histogramBoundsMs are the finite upper bounds of Snapshot.Histogram's
// bins, in milliseconds. A final bin beyond the last of these holds every
// latency over 10s (see Bucket's doc comment for how it is encoded).
var histogramBoundsMs = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

// progressInterval is how often Run calls progress while a test is running.
const progressInterval = 500 * time.Millisecond

// activityInterval is the minimum gap between two calls to Config.Activity.
const activityInterval = 1 * time.Second

// Run executes a load test against c.URL until it finishes, is stopped by
// ctx, or hits one of its own stop conditions (duration elapsed, request
// count reached, or MaxRequests reached). progress, if non-nil, is called
// roughly every 500ms with the run's state so far, and once more at the end
// with Done set.
//
// Cancelling ctx stops the run early: Run returns the final snapshot with
// Phase "stopped" and a nil error — cancellation is the Stop button, not a
// failure.
func Run(ctx context.Context, c Config, progress func(Snapshot)) (Snapshot, error) {
	if err := Validate(c); err != nil {
		return Snapshot{}, err
	}

	method := strings.ToUpper(c.Method)
	if method == "" {
		method = http.MethodGet
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	maxRequests := c.MaxRequests
	if maxRequests <= 0 {
		maxRequests = defaultMaxRequests
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost: c.Concurrency,
		MaxConnsPerHost:     0,
		ForceAttemptHTTP2:   true,
		TLSClientConfig: &tls.Config{
			RootCAs:            c.RootCAs,
			InsecureSkipVerify: c.InsecureSkipVerify,
		},
		DisableCompression: false,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start := time.Now()
	warmupDeadline := start.Add(c.WarmUp)
	measuredStart := warmupDeadline

	requestsMode := c.Duration == 0
	targetRequests := c.Requests
	if targetRequests <= 0 || targetRequests > maxRequests {
		targetRequests = maxRequests
	}

	st := newStats()
	var issued int64

	var wg sync.WaitGroup
	for i := 0; i < c.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				warm := time.Now().Before(warmupDeadline)
				if !warm {
					if requestsMode {
						if atomic.AddInt64(&issued, 1) > int64(targetRequests) {
							return
						}
					} else {
						if time.Since(measuredStart) >= c.Duration {
							return
						}
						if atomic.AddInt64(&issued, 1) > int64(maxRequests) {
							return
						}
					}
				}

				reqStart := time.Now()
				status, nbytes, err := doRequest(ctx, client, method, c.URL, c.Headers, c.Body)
				dur := time.Since(reqStart)

				if warm {
					continue
				}
				st.record(dur, status, nbytes, err, time.Since(measuredStart))
			}
		}()
	}

	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()

	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	var lastActivity time.Time

waitLoop:
	for {
		select {
		case <-workersDone:
			break waitLoop
		case now := <-ticker.C:
			if progress != nil {
				progress(st.snapshot(measuredStart, currentPhase(now, warmupDeadline), false))
			}
			if c.Activity != nil && now.Sub(lastActivity) >= activityInterval {
				c.Activity()
				lastActivity = now
			}
		}
	}

	phase := "done"
	if ctx.Err() != nil {
		phase = "stopped"
	}
	final := st.snapshot(measuredStart, phase, true)
	if progress != nil {
		progress(final)
	}
	return final, nil
}

func currentPhase(now, warmupDeadline time.Time) string {
	if now.Before(warmupDeadline) {
		return "warmup"
	}
	return "running"
}

// doRequest sends one request and fully drains the response body so the
// connection can be reused. status is 0 and err is non-nil on a transport
// failure (timeout, connection refused, etc).
func doRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body string) (status int, bytesIn int64, err error) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return 0, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	n, _ := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, n, nil
}

// secondAgg accumulates the requests completed during one second of the
// measured phase.
type secondAgg struct {
	count     int
	errors    int
	latencies []float64
}

// stats accumulates every completed, counted request. All access goes
// through mu so Run's worker goroutines and the periodic snapshot can share
// it safely.
type stats struct {
	mu sync.Mutex

	requests int
	errors   int
	bytesIn  int64

	latencies []float64 // ms, in completion order

	status map[string]int

	errorSamples []string
	errorSeen    map[string]bool

	seconds   map[int]*secondAgg
	maxSecond int
}

func newStats() *stats {
	return &stats{
		status:    map[string]int{},
		errorSeen: map[string]bool{},
		seconds:   map[int]*secondAgg{},
	}
}

// record adds one completed request. elapsed is the time since the
// measured phase started, used to place the request in the right
// second-of-run bucket for the timeline.
func (s *stats) record(dur time.Duration, status int, bytesIn int64, err error, elapsed time.Duration) {
	ms := float64(dur) / float64(time.Millisecond)

	isErr := false
	var statusKey string
	if err != nil {
		isErr = true
		statusKey = "error"
	} else {
		statusKey = strconv.Itoa(status)
		if status >= 500 {
			isErr = true
		}
	}

	sec := int(elapsed.Seconds())
	if sec < 0 {
		sec = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests++
	if isErr {
		s.errors++
	}
	s.status[statusKey]++
	s.bytesIn += bytesIn
	s.latencies = append(s.latencies, ms)

	if err != nil {
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if !s.errorSeen[msg] && len(s.errorSamples) < 5 {
			s.errorSeen[msg] = true
			s.errorSamples = append(s.errorSamples, msg)
		}
	}

	agg := s.seconds[sec]
	if agg == nil {
		agg = &secondAgg{}
		s.seconds[sec] = agg
	}
	agg.count++
	if isErr {
		agg.errors++
	}
	agg.latencies = append(agg.latencies, ms)
	if sec > s.maxSecond {
		s.maxSecond = sec
	}
}

// snapshot builds a Snapshot of the accumulated results.
func (s *stats) snapshot(measuredStart time.Time, phase string, done bool) Snapshot {
	s.mu.Lock()
	latencies := append([]float64(nil), s.latencies...)
	statusCopy := make(map[string]int, len(s.status))
	for k, v := range s.status {
		statusCopy[k] = v
	}
	errorSamples := append([]string(nil), s.errorSamples...)
	requests := s.requests
	errs := s.errors
	bytesIn := s.bytesIn
	maxSecond := s.maxSecond

	timeline := make([]Point, 0, maxSecond+1)
	for sec := 0; sec <= maxSecond; sec++ {
		agg := s.seconds[sec]
		if agg == nil {
			continue
		}
		sorted := append([]float64(nil), agg.latencies...)
		sort.Float64s(sorted)
		timeline = append(timeline, Point{
			T:      float64(sec),
			RPS:    round2(float64(agg.count)),
			P95:    round2(percentile(sorted, 95)),
			Errors: agg.errors,
		})
	}
	s.mu.Unlock()

	sort.Float64s(latencies)

	elapsed := time.Since(measuredStart).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}

	var minMs, maxMs, meanMs, p50, p90, p95, p99 float64
	if len(latencies) > 0 {
		minMs = latencies[0]
		maxMs = latencies[len(latencies)-1]
		sum := 0.0
		for _, v := range latencies {
			sum += v
		}
		meanMs = sum / float64(len(latencies))
		p50 = percentile(latencies, 50)
		p90 = percentile(latencies, 90)
		p95 = percentile(latencies, 95)
		p99 = percentile(latencies, 99)
	}

	rps := 0.0
	if elapsed > 0 {
		rps = float64(requests) / elapsed
	}

	return Snapshot{
		Elapsed:      round2(elapsed),
		Phase:        phase,
		Done:         done,
		Requests:     requests,
		Errors:       errs,
		RPS:          round2(rps),
		MinMs:        round2(minMs),
		MaxMs:        round2(maxMs),
		MeanMs:       round2(meanMs),
		P50Ms:        round2(p50),
		P90Ms:        round2(p90),
		P95Ms:        round2(p95),
		P99Ms:        round2(p99),
		Status:       statusCopy,
		ErrorSamples: errorSamples,
		Histogram:    buildHistogram(latencies),
		Timeline:     timeline,
		BytesIn:      bytesIn,
	}
}

// percentile returns the p-th percentile (0..100) of sorted, a slice
// already in ascending order, using linear interpolation between the two
// nearest ranks.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo] + (sorted[hi]-sorted[lo])*frac
}

// buildHistogram bins sorted-or-not latencies (ms) into Snapshot.Histogram's
// fixed buckets. Each request falls into exactly one bucket, so the counts
// sum to len(latencies).
func buildHistogram(latencies []float64) []Bucket {
	buckets := make([]Bucket, len(histogramBoundsMs)+1)
	for i, b := range histogramBoundsMs {
		buckets[i].LeMs = b
	}
	buckets[len(buckets)-1].LeMs = -1 // +Inf

	for _, ms := range latencies {
		placed := false
		for i, b := range histogramBoundsMs {
			if ms <= b {
				buckets[i].Count++
				placed = true
				break
			}
		}
		if !placed {
			buckets[len(buckets)-1].Count++
		}
	}
	return buckets
}

// round2 rounds x to 2 decimal places.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}
