package phpver

import "testing"

func TestParseSelector(t *testing.T) {
	cases := []struct {
		in      string
		want    Selector
		wantErr bool
	}{
		{"8", Selector{8, -1, -1}, false},
		{"8.3", Selector{8, 3, -1}, false},
		{"8.3.26", Selector{8, 3, 26}, false},
		{"", Selector{}, true},
		{"abc", Selector{}, true},
		{"8.3.26.1", Selector{}, true},
		{"-1", Selector{}, true},
	}
	for _, c := range cases {
		got, err := ParseSelector(c.in)
		if c.wantErr != (err != nil) {
			t.Fatalf("ParseSelector(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Fatalf("ParseSelector(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestSelectorMatches(t *testing.T) {
	sel, _ := ParseSelector("8.3")
	if !sel.Matches("8.3.26") {
		t.Fatal("8.3 should match 8.3.26")
	}
	if sel.Matches("8.2.20") {
		t.Fatal("8.3 should not match 8.2.20")
	}
	major, _ := ParseSelector("8")
	if !major.Matches("8.2.20") {
		t.Fatal("8 should match 8.2.20")
	}
	exact, _ := ParseSelector("8.3.26")
	if exact.Matches("8.3.27") {
		t.Fatal("8.3.26 should not match 8.3.27")
	}
}

func TestCompareNumeric(t *testing.T) {
	if Compare("8.3.2", "8.3.10") != -1 {
		t.Fatal("8.3.2 must sort before 8.3.10 (numeric, not lexicographic)")
	}
	if Compare("8.10.0", "8.9.0") != 1 {
		t.Fatal("8.10.0 must sort after 8.9.0")
	}
	if Compare("8.3.1", "8.3.1") != 0 {
		t.Fatal("equal versions must compare 0")
	}
}

func TestFcgiPort(t *testing.T) {
	cases := map[string]int{"8.3.26": 9083, "8.2.20": 9082, "7.4.33": 9074}
	for v, want := range cases {
		got, err := FcgiPort(v)
		if err != nil || got != want {
			t.Fatalf("FcgiPort(%s) = %d, %v; want %d", v, got, err, want)
		}
	}
	if _, err := FcgiPort("8"); err == nil {
		t.Fatal("FcgiPort should reject a bare major version")
	}
}
