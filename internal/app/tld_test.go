package app

import "testing"

func TestValidateTLD(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"test", "test", false},
		{"LOCAL", "local", false},
		{"  local  ", "local", false},
		{".local.", "local", false},
		{"my-app", "my-app", false},
		{"", "", true},
		{".", "", true},
		{"has a space", "", true},
		{"under_score", "", true},
		{"dots.in.it", "", true},
		{"Ünïcode", "", true},
	}
	for _, c := range cases {
		got, err := ValidateTLD(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ValidateTLD(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ValidateTLD(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateTLD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
