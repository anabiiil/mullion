package app

import (
	"reflect"
	"testing"
)

func TestShouldSelfHeal(t *testing.T) {
	cases := []struct {
		name    string
		version string
		stopped bool
		want    bool
	}{
		{"not installed, not stopped", "", false, false},
		{"not installed, stopped flag left over", "", true, false},
		{"installed, running", "8.4.11", false, true},
		{"installed, user stopped it", "8.4.11", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldSelfHeal(c.version, c.stopped); got != c.want {
				t.Errorf("shouldSelfHeal(%q, %v) = %v, want %v", c.version, c.stopped, got, c.want)
			}
		})
	}
}

func TestFilterLongTermMongoSeries(t *testing.T) {
	in := []string{"8.3", "8.2", "8.0", "7.0", "6.0"}
	want := []string{"8.0", "7.0", "6.0"}
	got := filterLongTermMongoSeries(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filterLongTermMongoSeries(%v) = %v, want %v", in, got, want)
	}
}

func TestFilterLongTermMongoSeriesEmpty(t *testing.T) {
	if got := filterLongTermMongoSeries(nil); len(got) != 0 {
		t.Errorf("filterLongTermMongoSeries(nil) = %v, want empty", got)
	}
}
