package cmd

import "testing"

func TestIdleMinutes(t *testing.T) {
	for in, want := range map[string]int{"": 0, "5m": 5, "5": 5, "1h30m": 90, "90s": 2, "24h": 1440} {
		if got, err := idleMinutes(in); err != nil || got != want {
			t.Errorf("idleMinutes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"10s", "25h", "soon", "0"} {
		if _, err := idleMinutes(bad); err == nil {
			t.Errorf("idleMinutes(%q) should fail", bad)
		}
	}
}
