package cmd

import (
	"slices"
	"testing"

	"pm/internal/phpver"
)

func TestPhpUpdateCheckCode(t *testing.T) {
	if got := phpUpdateCheckCode(nil); got != 0 {
		t.Errorf("no updates: exit %d, want 0", got)
	}
	if got := phpUpdateCheckCode([]string{"8.4.23"}); got != 10 {
		t.Errorf("updates: exit %d, want 10", got)
	}
}

func TestSelectPhpUpdates(t *testing.T) {
	ups := map[string]phpver.Update{
		"8.4.23": {From: "8.4.23", To: "8.4.27"},
		"8.4.20": {From: "8.4.20", To: "8.4.27"},
		"8.3.1":  {From: "8.3.1", To: "8.3.9"},
	}
	cases := []struct {
		args []string
		want []string
	}{
		{nil, []string{"8.3.1", "8.4.20", "8.4.23"}},
		{[]string{"8.4"}, []string{"8.4.20", "8.4.23"}},
		{[]string{"8.4.23"}, []string{"8.4.23"}},
		{[]string{"8.2"}, nil},
	}
	for _, c := range cases {
		got, err := selectPhpUpdates(ups, c.args)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("selectPhpUpdates(%v) = %v, %v; want %v", c.args, got, err, c.want)
		}
	}
	if _, err := selectPhpUpdates(ups, []string{"8"}); err == nil {
		t.Error("a major-only selector was accepted")
	}
	if _, err := selectPhpUpdates(ups, []string{"x"}); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestUpdateHint(t *testing.T) {
	if got := updateHint(phpver.Update{}); got != "" {
		t.Errorf("no update: %q", got)
	}
	if got := updateHint(phpver.Update{From: "8.4.23", To: "8.4.27"}); got != "  (update: 8.4.27)" {
		t.Errorf("update: %q", got)
	}
	if got := updateHint(phpver.Update{From: "8.4.23", To: "8.4.27", Security: true}); got != "  (update: 8.4.27, security)" {
		t.Errorf("security update: %q", got)
	}
}
