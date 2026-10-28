//go:build !windows

package workers

import (
	"testing"
	"time"
)

func TestHistoryRoundTrip(t *testing.T) {
	paths := testPaths(t)
	if got := LoadHistory(paths, "demo", "worker-1"); got != nil {
		t.Fatalf("LoadHistory on missing file = %v, want nil", got)
	}
	samples := make([]Sample, 0, HistoryMax+10)
	for i := 0; i < HistoryMax+10; i++ {
		samples = append(samples, Sample{T: int64(i), RSS: int64(i) * 1024, CPU: 1.5, Procs: 2})
	}
	if err := SaveHistory(paths, "demo", "worker-1", samples); err != nil {
		t.Fatal(err)
	}
	got := LoadHistory(paths, "demo", "worker-1")
	if len(got) != HistoryMax {
		t.Fatalf("len(got) = %d, want %d (ring trimmed)", len(got), HistoryMax)
	}
	if got[len(got)-1].T != samples[len(samples)-1].T {
		t.Fatalf("ring kept the wrong tail: last T = %d, want %d", got[len(got)-1].T, samples[len(samples)-1].T)
	}
}

func TestParsePS(t *testing.T) {
	fixture := "" +
		"  123    1  123  10240   0.5\n" +
		"  456  123  123   2048   0.1\n" +
		"  789    1  789   4096   0.0\n" +
		"garbage line\n" +
		"\n"
	rows := parsePS(fixture)
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3: %+v", len(rows), rows)
	}
	want := psRow{pid: 123, ppid: 1, pgid: 123, rssKB: 10240, pcpu: 0.5}
	if rows[0] != want {
		t.Fatalf("rows[0] = %+v, want %+v", rows[0], want)
	}
}

func TestSampleTreesRealProcess(t *testing.T) {
	cmd := ShellCommand("sleep 5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { KillTree(pid) })

	waitFor(t, "ps to see the child", func() bool {
		_, ok := SampleTrees([]int{pid})[pid]
		return ok
	})

	samples := SampleTrees([]int{pid})
	s, ok := samples[pid]
	if !ok {
		t.Fatalf("SampleTrees did not find pid %d", pid)
	}
	if s.RSS <= 0 {
		t.Fatalf("RSS = %d, want > 0", s.RSS)
	}
	if s.Procs < 1 {
		t.Fatalf("Procs = %d, want >= 1", s.Procs)
	}
}

func TestSamplerSampleNowAndHistory(t *testing.T) {
	paths := testPaths(t)
	cmd := ShellCommand("sleep 5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { KillTree(pid) })

	s := NewSampler(paths, time.Second)
	targets := []Target{{Site: "demo", ID: "worker-1", Pid: pid}}

	waitFor(t, "a sample to land", func() bool {
		s.SampleNow(targets)
		return len(s.History("demo", "worker-1")) > 0
	})

	hist := s.History("demo", "worker-1")
	if hist[len(hist)-1].RSS <= 0 {
		t.Fatalf("last sample RSS = %d, want > 0", hist[len(hist)-1].RSS)
	}

	// A pid change (restart) should be recorded, and sampling continues
	// to append to the same ring.
	targets[0].Pid = pid // same pid: no restart recorded
	s.SampleNow(targets)
	if got := s.Restarts("demo", "worker-1"); len(got) != 0 {
		t.Fatalf("Restarts = %v, want none for an unchanged pid", got)
	}
	targets[0].Pid = 2015120 // fake restart: a pid ps won't find
	s.SampleNow(targets)
	if got := s.Restarts("demo", "worker-1"); len(got) != 1 {
		t.Fatalf("Restarts = %v, want 1 entry after a pid change", got)
	}
}

// linear builds n samples at the given interval, with RSS starting at
// startMB and growing by stepMB each sample (0 for flat).
func linear(n int, interval time.Duration, startMB, stepMB float64) []Sample {
	out := make([]Sample, n)
	for i := 0; i < n; i++ {
		out[i] = Sample{
			T:   int64(i) * int64(interval/time.Second),
			RSS: int64((startMB + stepMB*float64(i)) * (1 << 20)),
		}
	}
	return out
}

func TestDetectLeakSteadyGrowth(t *testing.T) {
	// 15 minutes at 10s intervals, growing ~80MB/hour.
	samples := linear(90, 10*time.Second, 100, 0.2222)
	leak := DetectLeak(samples)
	if !leak.Suspected {
		t.Fatalf("leak = %+v, want Suspected", leak)
	}
	if leak.SlopeMBHour < LeakMinSlopeMBHour {
		t.Fatalf("SlopeMBHour = %v, want >= %v", leak.SlopeMBHour, LeakMinSlopeMBHour)
	}
	if leak.R2 < LeakMinR2 {
		t.Fatalf("R2 = %v, want >= %v", leak.R2, LeakMinR2)
	}
}

func TestDetectLeakFlatNoisy(t *testing.T) {
	samples := linear(90, 10*time.Second, 100, 0)
	// Small symmetric noise that returns to baseline.
	for i := range samples {
		if i%2 == 0 {
			samples[i].RSS += 1 << 20
		} else {
			samples[i].RSS -= 1 << 20
		}
	}
	leak := DetectLeak(samples)
	if leak.Suspected {
		t.Fatalf("leak = %+v, want not suspected", leak)
	}
}

func TestDetectLeakSawtooth(t *testing.T) {
	samples := make([]Sample, 0, 90)
	samples = append(samples, linear(45, 10*time.Second, 100, 1.111)...) // up to ~150MB
	down := linear(45, 10*time.Second, 150, -0.889)                      // back down to ~110MB
	for i, s := range down {
		s.T = samples[len(samples)-1].T + int64(i+1)*10
		samples = append(samples, s)
	}
	leak := DetectLeak(samples)
	if leak.Suspected {
		t.Fatalf("leak = %+v, want not suspected (sawtooth)", leak)
	}
}

func TestDetectLeakTooShort(t *testing.T) {
	samples := linear(15, 10*time.Second, 100, 1)
	leak := DetectLeak(samples)
	if leak.Suspected {
		t.Fatalf("leak = %+v, want not suspected (too short)", leak)
	}
	if leak.Reason == "" {
		t.Fatal("Reason is empty")
	}
}

func TestDetectLeakRestartThenFlat(t *testing.T) {
	samples := linear(30, 10*time.Second, 100, 0.333) // grows to ~110MB over 5min
	crash := Sample{T: samples[len(samples)-1].T + 10, RSS: int64(20 * (1 << 20))}
	samples = append(samples, crash)
	post := linear(70, 10*time.Second, 20, 0) // flat ~20MB for ~11.7min
	for i, s := range post {
		s.T = crash.T + int64(i+1)*10
		// tiny noise, well under the drop threshold.
		if i%2 == 0 {
			s.RSS += 1 << 18
		}
		samples = append(samples, s)
	}
	leak := DetectLeak(samples)
	if leak.Suspected {
		t.Fatalf("leak = %+v, want not suspected (post-restart is flat)", leak)
	}
	if leak.SpanMin > 12 {
		t.Fatalf("SpanMin = %v, want the post-restart span only (~11.7min), not the whole history", leak.SpanMin)
	}
}
