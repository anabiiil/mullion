// Worker health: process-tree resource sampling, a small on-disk
// history ring per worker, and simple memory-leak detection over that
// history. SampleTrees itself is platform-specific (health_other.go,
// health_windows.go); everything else here is shared.
package workers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"pm/internal/pmdir"
)

// Sample is one point-in-time measurement of a worker's whole process
// tree (the worker itself plus anything it spawned).
type Sample struct {
	T     int64   `json:"t"`   // unix seconds
	RSS   int64   `json:"rss"` // bytes, summed over the tree
	CPU   float64 `json:"cpu"` // percent of one core, summed over the tree
	Procs int     `json:"procs"`
}

// HistoryMax is how many samples a worker's ring keeps: 1 hour at the
// sampler's default 10s interval.
const HistoryMax = 360

// HistoryPath is where a worker's persisted sample ring lives.
func HistoryPath(paths pmdir.Paths, site, id string) string {
	return filepath.Join(paths.Home, "perf", site, "workers", id+".json")
}

// LoadHistory reads a worker's persisted ring ("" / missing / corrupt
// file all come back as nil, not an error — history is best-effort).
func LoadHistory(paths pmdir.Paths, site, id string) []Sample {
	data, err := os.ReadFile(HistoryPath(paths, site, id))
	if err != nil {
		return nil
	}
	var samples []Sample
	if err := json.Unmarshal(data, &samples); err != nil {
		return nil
	}
	return samples
}

// SaveHistory writes a worker's ring atomically (tmp + rename), keeping
// only the last HistoryMax samples.
func SaveHistory(paths pmdir.Paths, site, id string, s []Sample) error {
	if len(s) > HistoryMax {
		s = s[len(s)-HistoryMax:]
	}
	path := HistoryPath(paths, site, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Target is one worker to sample: its live pid, keyed by site/id.
type Target struct {
	Site string
	ID   string
	Pid  int
}

// ringKey identifies one worker's ring in the sampler's maps.
type ringKey struct{ Site, ID string }

// Sampler periodically measures a set of workers' process trees, keeps
// an in-memory ring per worker (seeded from disk on first sight) and
// persists it now and then. Safe for concurrent use.
type Sampler struct {
	paths    pmdir.Paths
	interval time.Duration

	mu       sync.Mutex
	rings    map[ringKey][]Sample
	restarts map[ringKey][]int64
	lastPid  map[ringKey]int
	lastSave time.Time
}

// NewSampler creates a sampler that measures every interval.
func NewSampler(paths pmdir.Paths, interval time.Duration) *Sampler {
	return &Sampler{
		paths:    paths,
		interval: interval,
		rings:    map[ringKey][]Sample{},
		restarts: map[ringKey][]int64{},
		lastPid:  map[ringKey]int{},
	}
}

// Run samples every interval until ctx ends. targets() is called each
// tick to get the current set of running workers; when it returns none,
// no `ps` (or Toolhelp32 snapshot) is taken that tick.
func (s *Sampler) Run(ctx context.Context, targets func() []Target) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.persistAll()
			return
		case <-ticker.C:
			s.SampleNow(targets())
		}
	}
}

// SampleNow runs one immediate measurement tick (used by tests and when
// a UI panel opens and wants a reading right away).
func (s *Sampler) SampleNow(targets []Target) {
	if len(targets) == 0 {
		return
	}
	pids := make([]int, 0, len(targets))
	for _, t := range targets {
		pids = append(pids, t.Pid)
	}
	measured := SampleTrees(pids)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range targets {
		key := ringKey{Site: t.Site, ID: t.ID}
		if _, seen := s.rings[key]; !seen {
			s.rings[key] = LoadHistory(s.paths, t.Site, t.ID)
		}
		if lp, ok := s.lastPid[key]; ok && lp != 0 && lp != t.Pid {
			s.restarts[key] = append(s.restarts[key], time.Now().Unix())
		}
		s.lastPid[key] = t.Pid

		samp, ok := measured[t.Pid]
		if !ok {
			continue // pid vanished mid-tick (just exited, etc.)
		}
		ring := append(s.rings[key], samp)
		if len(ring) > HistoryMax {
			ring = ring[len(ring)-HistoryMax:]
		}
		s.rings[key] = ring
	}
	if time.Since(s.lastSave) >= time.Minute {
		s.persistAllLocked()
		s.lastSave = time.Now()
	}
}

// History returns a copy of one worker's in-memory sample ring.
func (s *Sampler) History(site, id string) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring := s.rings[ringKey{Site: site, ID: id}]
	out := make([]Sample, len(ring))
	copy(out, ring)
	return out
}

// Restarts returns the unix-second times this worker's pid changed
// (crash or manual restart) since the sampler started watching it.
func (s *Sampler) Restarts(site, id string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.restarts[ringKey{Site: site, ID: id}]
	out := make([]int64, len(rs))
	copy(out, rs)
	return out
}

func (s *Sampler) persistAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistAllLocked()
}

func (s *Sampler) persistAllLocked() {
	for key, ring := range s.rings {
		_ = SaveHistory(s.paths, key.Site, key.ID, ring)
	}
}

// ---- leak detection --------------------------------------------------

// Thresholds behind DetectLeak's verdict.
const (
	LeakMinSpanMinutes = 10.0
	LeakMinSamples     = 20
	LeakMinSlopeMBHour = 8.0
	LeakMinR2          = 0.7
	LeakMinGrowthMB    = 5.0

	// leakDropFraction: a sample under this fraction of the running max
	// counts as a drop (GC, or a restart we didn't already cut before).
	leakDropFraction = 0.95
	// leakRestartRSSDrop: a single-step RSS fall by more than this
	// fraction marks a restart gap.
	leakRestartRSSDrop = 0.30
	// leakRestartGapFactor: a gap more than this many times the median
	// sample interval also marks a restart gap (sampler was paused/down).
	leakRestartGapFactor = 5.0
)

// Leak is DetectLeak's verdict for one worker's sample history.
type Leak struct {
	Suspected   bool    `json:"suspected"`
	SlopeMBHour float64 `json:"slopeMBPerHour"`
	SpanMin     float64 `json:"spanMin"`
	R2          float64 `json:"r2"`
	// Reason is a plain-language explanation for the UI, filled in
	// whether or not a leak is suspected.
	Reason string `json:"reason"`
}

// DetectLeak looks for steady, sustained memory growth in a worker's
// sample history. It only considers samples since the last restart gap
// (a big time jump, or an RSS drop that looks like a restart), needs at
// least 10 minutes and 20 samples of that, and flags a leak on a
// confident (R² >= 0.7) upward slope of at least 8 MB/hour with no
// later drop and at least 5 MB of total growth.
func DetectLeak(samples []Sample) Leak {
	if cut := restartCutIndex(samples); cut > 0 {
		samples = samples[cut:]
	}
	if len(samples) < LeakMinSamples {
		return Leak{Reason: "need 10 minutes of samples"}
	}
	spanMin := float64(samples[len(samples)-1].T-samples[0].T) / 60
	if spanMin < LeakMinSpanMinutes {
		return Leak{SpanMin: spanMin, Reason: "need 10 minutes of samples"}
	}

	slopeBPS, r2 := linRegRSS(samples)
	slopeMBHour := slopeBPS * 3600 / (1 << 20)
	growthMB := float64(samples[len(samples)-1].RSS-samples[0].RSS) / (1 << 20)

	leak := Leak{SlopeMBHour: slopeMBHour, SpanMin: spanMin, R2: r2}
	switch {
	case hasDrop(samples):
		leak.Reason = "memory dropped by " + formatPercent(dropPercent(samples)) + "% — garbage collected / restarted"
	case slopeMBHour >= LeakMinSlopeMBHour && r2 >= LeakMinR2 && growthMB >= LeakMinGrowthMB:
		leak.Suspected = true
		leak.Reason = "memory is growing steadily, about " + formatPercent(slopeMBHour) + " MB/hour"
	default:
		leak.Reason = "memory is flat"
	}
	return leak
}

func formatPercent(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// restartCutIndex finds the index just after the LAST restart-looking
// gap in samples (0 if there is none): a time jump more than
// leakRestartGapFactor times the median interval, or a single-step RSS
// fall of more than leakRestartRSSDrop.
func restartCutIndex(samples []Sample) int {
	if len(samples) < 3 {
		return 0
	}
	diffs := make([]int64, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		diffs = append(diffs, samples[i].T-samples[i-1].T)
	}
	med := medianInt64(diffs)

	cut := 0
	for i := 1; i < len(samples); i++ {
		dt := samples[i].T - samples[i-1].T
		gapJump := med > 0 && float64(dt) > leakRestartGapFactor*float64(med)
		rssDrop := samples[i-1].RSS > 0 &&
			float64(samples[i].RSS) < float64(samples[i-1].RSS)*(1-leakRestartRSSDrop)
		if gapJump || rssDrop {
			cut = i
		}
	}
	return cut
}

func medianInt64(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// hasDrop reports whether, after the window's first 10%, RSS ever falls
// under leakDropFraction of the running max seen so far — a sawtooth
// (grow then GC/restart) rather than a steady leak.
func hasDrop(samples []Sample) bool {
	if len(samples) == 0 {
		return false
	}
	start := len(samples) / 10
	runningMax := samples[0].RSS
	for i, s := range samples {
		if s.RSS > runningMax {
			runningMax = s.RSS
		}
		if i >= start && float64(s.RSS) < leakDropFraction*float64(runningMax) {
			return true
		}
	}
	return false
}

// dropPercent is how far RSS fell from its peak to its trough, as a
// percentage of the peak — used only for the UI explanation text.
func dropPercent(samples []Sample) float64 {
	max, min := samples[0].RSS, samples[0].RSS
	for _, s := range samples {
		if s.RSS > max {
			max = s.RSS
		}
		if s.RSS < min {
			min = s.RSS
		}
	}
	if max == 0 {
		return 0
	}
	return float64(max-min) / float64(max) * 100
}

// linRegRSS fits a least-squares line of RSS (bytes) over time (seconds
// since the first sample) and returns its slope (bytes/second) and R².
func linRegRSS(samples []Sample) (slope, r2 float64) {
	n := float64(len(samples))
	if n < 2 {
		return 0, 0
	}
	t0 := samples[0].T
	var sumX, sumY, sumXY, sumXX float64
	for _, s := range samples {
		x := float64(s.T - t0)
		y := float64(s.RSS)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, 0
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept := (sumY - slope*sumX) / n

	meanY := sumY / n
	var ssRes, ssTot float64
	for _, s := range samples {
		x := float64(s.T - t0)
		y := float64(s.RSS)
		pred := slope*x + intercept
		ssRes += (y - pred) * (y - pred)
		ssTot += (y - meanY) * (y - meanY)
	}
	if ssTot == 0 {
		if ssRes == 0 {
			return slope, 1
		}
		return slope, 0
	}
	return slope, 1 - ssRes/ssTot
}
