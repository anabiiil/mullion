package workers

import "testing"

func TestParseQueueLogNewFormat(t *testing.T) {
	log := "" +
		"  2026-10-28 10:00:00 App\\Jobs\\SendMail ......................... RUNNING\n" +
		"  2026-10-28 10:00:00 App\\Jobs\\SendMail ......................... 120.50ms DONE\n" +
		"  2026-10-28 10:00:01 App\\Jobs\\Charge ........................... 1s FAIL\n" +
		"  2026-10-28 10:00:02 App\\Jobs\\Charge ........................... 2.5s DONE\n" +
		"  2026-10-28 10:00:03 App\\Jobs\\Report ........................... 1m 3s DONE\n" +
		"---- 2026-10-28T10:00:04Z: mullion restarting worker \"Queue\": `php artisan queue:work` ----\n" +
		"  2026-10-28 10:00:05 App\\Jobs\\SendMail ......................... RUNNING\n"

	stats := ParseQueueLog(log)
	if stats.Running != "App\\Jobs\\SendMail" {
		t.Fatalf("Running = %q, want the still-RUNNING job", stats.Running)
	}
	if stats.Lines != 6 {
		t.Fatalf("Lines = %d, want 6 (marker line excluded)", stats.Lines)
	}

	byClass := map[string]JobStat{}
	for _, j := range stats.Jobs {
		byClass[j.Class] = j
	}

	mail := byClass["App\\Jobs\\SendMail"]
	if mail.Done != 1 || mail.Failed != 0 || mail.AvgMs != 120.5 || mail.MaxMs != 120.5 {
		t.Fatalf("SendMail = %+v", mail)
	}

	charge := byClass["App\\Jobs\\Charge"]
	if charge.Done != 1 || charge.Failed != 1 {
		t.Fatalf("Charge = %+v", charge)
	}
	wantAvg := (1000.0 + 2500.0) / 2
	if charge.AvgMs != wantAvg || charge.MaxMs != 2500 {
		t.Fatalf("Charge avg/max = %v/%v, want %v/2500", charge.AvgMs, charge.MaxMs, wantAvg)
	}
	if charge.LastFail == "" {
		t.Fatal("Charge.LastFail is empty")
	}

	report := byClass["App\\Jobs\\Report"]
	if report.Done != 1 || report.MaxMs != 63000 {
		t.Fatalf("Report = %+v, want a 1m3s=63000ms duration", report)
	}
}

func TestParseQueueLogANSICodes(t *testing.T) {
	log := "  2026-10-28 10:00:00 \x1b[33mApp\\Jobs\\X\x1b[39m \x1b[33m.....\x1b[39m \x1b[32m50ms DONE\x1b[39m\n"
	stats := ParseQueueLog(log)
	if len(stats.Jobs) != 1 || stats.Jobs[0].Class != "App\\Jobs\\X" || stats.Jobs[0].Done != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Jobs[0].AvgMs != 50 {
		t.Fatalf("AvgMs = %v, want 50", stats.Jobs[0].AvgMs)
	}
}

func TestParseQueueLogOldFormat(t *testing.T) {
	log := "" +
		"[2026-10-28 10:00:00][abc123] Processing: App\\Jobs\\X\n" +
		"[2026-10-28 10:00:02][abc123] Processed:  App\\Jobs\\X\n" +
		"[2026-10-28 10:00:03][def456] Processing: App\\Jobs\\Y\n" +
		"[2026-10-28 10:00:04][def456] Failed:     App\\Jobs\\Y\n" +
		"[2026-10-28 10:00:05][ghi789] Processing: App\\Jobs\\Z\n"

	stats := ParseQueueLog(log)
	if stats.Running != "App\\Jobs\\Z" {
		t.Fatalf("Running = %q, want the dangling Processing line's job", stats.Running)
	}

	byClass := map[string]JobStat{}
	for _, j := range stats.Jobs {
		byClass[j.Class] = j
	}
	x := byClass["App\\Jobs\\X"]
	if x.Done != 1 || x.AvgMs != 2000 {
		t.Fatalf("X = %+v, want a 2s=2000ms duration", x)
	}
	y := byClass["App\\Jobs\\Y"]
	if y.Failed != 1 || y.AvgMs != 1000 {
		t.Fatalf("Y = %+v, want a 1s=1000ms duration", y)
	}
}

func TestParseQueueLogRepeatedFailures(t *testing.T) {
	log := "" +
		"  2026-10-28 10:00:00 App\\Jobs\\Flaky .............................. 1s FAIL\n" +
		"  2026-10-28 10:00:01 App\\Jobs\\Flaky .............................. 1s FAIL\n" +
		"  2026-10-28 10:00:02 App\\Jobs\\Flaky .............................. 1s FAIL\n" +
		"  2026-10-28 10:00:03 App\\Jobs\\Stable ............................. 1s DONE\n"

	stats := ParseQueueLog(log)
	if len(stats.Jobs) == 0 || !stats.Jobs[0].Repeated || stats.Jobs[0].Class != "App\\Jobs\\Flaky" {
		t.Fatalf("Jobs[0] = %+v, want Flaky sorted first as repeated", stats.Jobs[0])
	}
	if stats.Jobs[0].Failed != 3 {
		t.Fatalf("Failed = %d, want 3", stats.Jobs[0].Failed)
	}
}

func TestParseDurationTokens(t *testing.T) {
	cases := []struct {
		tokens []string
		want   float64
	}{
		{[]string{"120.50ms"}, 120.5},
		{[]string{"1s"}, 1000},
		{[]string{"2.5s"}, 2500},
		{[]string{"1m", "3s"}, 63000},
		{[]string{"1h", "2m"}, 3720000},
	}
	for _, c := range cases {
		got, ok := parseDurationTokens(c.tokens)
		if !ok || got != c.want {
			t.Errorf("parseDurationTokens(%v) = %v, %v; want %v, true", c.tokens, got, ok, c.want)
		}
	}
}

func TestParseQueueLogIgnoresMarkerAndEmptyLines(t *testing.T) {
	log := "\n---- start ----\n\n  2026-10-28 10:00:00 App\\Jobs\\X .............................. 1s DONE\n\n"
	stats := ParseQueueLog(log)
	if stats.Lines != 1 {
		t.Fatalf("Lines = %d, want 1", stats.Lines)
	}
}
