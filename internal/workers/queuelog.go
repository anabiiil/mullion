// Parsing for Laravel `php artisan queue:work` output, so the panel can
// show per-job stats from a worker's plain log instead of a dedicated
// queue backend.
package workers

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"pm/internal/pmdir"
)

// JobStat is one job class's tally across a worker's log.
type JobStat struct {
	Class    string  `json:"class"`
	Done     int     `json:"done"`
	Failed   int     `json:"failed"`
	AvgMs    float64 `json:"avgMs"`
	MaxMs    float64 `json:"maxMs"`
	LastAt   string  `json:"lastAt"`             // timestamp text of the last line
	LastFail string  `json:"lastFail,omitempty"` // timestamp text of the last failure
	// Repeated: this job looks stuck failing — 3+ failures with at
	// least as many failures as successes, or its last 3 runs all failed.
	Repeated bool `json:"repeatedFailures"`
}

// QueueStats summarizes a worker's queue log.
type QueueStats struct {
	// Jobs is sorted: repeated failures first, then by failure count
	// descending, then by total run count descending.
	Jobs []JobStat `json:"jobs"`
	// Running is the class of a job that logged RUNNING/Processing with
	// no completion line after it yet (the last one, if there are
	// several — e.g. a crash mid-job).
	Running string `json:"running,omitempty"`
	// Lines is how many non-blank, non-marker lines were parsed.
	Lines int `json:"lines"`
}

var (
	ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
	// newFormatRe matches a Laravel 9+ line: timestamp, class, a run of
	// dots, then either RUNNING or "<duration> DONE|FAIL".
	newFormatRe = regexp.MustCompile(`^\s*(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\s+(\S+)\s+\.{2,}\s*(.*)$`)
	// oldFormatRe matches a Laravel <=8 line: [ts][id] Verb: Class.
	oldFormatRe = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\]\[([^\]]+)\]\s+(Processing|Processed|Failed):\s+(.+?)\s*$`)
	// durationUnitRe matches one duration token from CarbonInterval's
	// forHumans(short): "120.50ms", "1s", "2.5s", "1m", "2h".
	durationUnitRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(ms|h|m|s)$`)
)

// QueueStatsOf reads the tail of a worker's log and parses it.
func QueueStatsOf(paths pmdir.Paths, site, id string, lines int) QueueStats {
	return ParseQueueLog(LogTail(paths, site, id, lines))
}

// ParseQueueLog parses `php artisan queue:work` output in either the
// Laravel 9+ or the older [timestamp][id] format (both may appear in
// the same log, across an upgrade). Mullion's own "---- ... ----"
// marker lines are ignored.
func ParseQueueLog(text string) QueueStats {
	text = ansiRe.ReplaceAllString(text, "")

	type oldStart struct {
		t     time.Time
		class string
	}
	started := map[string]oldStart{}

	stats := map[string]*JobStat{}
	recent := map[string][]bool{} // last few outcomes per class, true = failed
	var order []string
	get := func(class string) *JobStat {
		js, ok := stats[class]
		if !ok {
			js = &JobStat{Class: class}
			stats[class] = js
			order = append(order, class)
		}
		return js
	}
	record := func(class, status string, ms float64, ts string) {
		js := get(class)
		switch status {
		case "DONE":
			js.Done++
		case "FAIL":
			js.Failed++
			js.LastFail = ts
		default:
			return
		}
		js.LastAt = ts
		n := js.Done + js.Failed
		js.AvgMs += (ms - js.AvgMs) / float64(n)
		if ms > js.MaxMs {
			js.MaxMs = ms
		}
		outs := append(recent[class], status == "FAIL")
		if len(outs) > 3 {
			outs = outs[len(outs)-3:]
		}
		recent[class] = outs
	}

	var running string
	lines := 0

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "----") {
			continue
		}
		lines++

		if m := newFormatRe.FindStringSubmatch(line); m != nil {
			ts, class, rest := m[1], m[2], strings.TrimSpace(m[3])
			if rest == "RUNNING" {
				running = class
				continue
			}
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				continue
			}
			status := fields[len(fields)-1]
			if status != "DONE" && status != "FAIL" {
				continue
			}
			ms, _ := parseDurationTokens(fields[:len(fields)-1])
			record(class, status, ms, ts)
			if running == class {
				running = ""
			}
			continue
		}

		if m := oldFormatRe.FindStringSubmatch(line); m != nil {
			ts, id, verb, class := m[1], m[2], m[3], strings.TrimSpace(m[4])
			t, _ := time.Parse("2006-01-02 15:04:05", ts)
			switch verb {
			case "Processing":
				started[id] = oldStart{t: t, class: class}
				running = class
			case "Processed", "Failed":
				status := "DONE"
				if verb == "Failed" {
					status = "FAIL"
				}
				var ms float64
				if s, ok := started[id]; ok {
					ms = float64(t.Sub(s.t).Milliseconds())
					delete(started, id)
				}
				record(class, status, ms, ts)
				if running == class {
					running = ""
				}
			}
			continue
		}
	}

	jobs := make([]JobStat, 0, len(order))
	for _, class := range order {
		js := *stats[class]
		outs := recent[class]
		last3Failed := len(outs) == 3 && outs[0] && outs[1] && outs[2]
		js.Repeated = (js.Failed >= 3 && js.Failed >= js.Done) || last3Failed
		jobs = append(jobs, js)
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].Repeated != jobs[j].Repeated {
			return jobs[i].Repeated
		}
		if jobs[i].Failed != jobs[j].Failed {
			return jobs[i].Failed > jobs[j].Failed
		}
		return jobs[i].Done+jobs[i].Failed > jobs[j].Done+jobs[j].Failed
	})

	return QueueStats{Jobs: jobs, Running: running, Lines: lines}
}

// parseDurationTokens sums CarbonInterval forHumans(short) tokens like
// "1m", "3s" (as split from "1m 3s") into milliseconds.
func parseDurationTokens(tokens []string) (float64, bool) {
	var total float64
	found := false
	for _, tok := range tokens {
		m := durationUnitRe.FindStringSubmatch(tok)
		if m == nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, false
		}
		switch m[2] {
		case "ms":
			total += v
		case "s":
			total += v * 1000
		case "m":
			total += v * 60000
		case "h":
			total += v * 3600000
		}
		found = true
	}
	return total, found
}
