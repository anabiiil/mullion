package app

import "testing"

func TestParseComposerVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Composer version 2.7.6 2024-06-10 22:11:12", "2.7.6"},
		{"Composer version 2.2.25\n", "2.2.25"},
		{"\x1b[32mComposer\x1b[39m version \x1b[33m2.10.3\x1b[39m 2026-08-27 13:34:23\r\n", "2.10.3"},
		{"", ""},
		{"garbage output", ""},
	}
	for _, c := range cases {
		if got := parseComposerVersion(c.in); got != c.want {
			t.Errorf("parseComposerVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDoctorStatus(t *testing.T) {
	if got := doctorStatus(true); got != DoctorOK {
		t.Errorf("doctorStatus(true) = %q, want %q", got, DoctorOK)
	}
	if got := doctorStatus(false); got != DoctorFail {
		t.Errorf("doctorStatus(false) = %q, want %q", got, DoctorFail)
	}
}

func TestCaddyfileMissingHosts(t *testing.T) {
	content := "some caddyfile text mentioning foo.test but not the other one"
	missing := caddyfileMissingHosts(content, []string{"foo.test", "bar.test"})
	if len(missing) != 1 || missing[0] != "bar.test" {
		t.Errorf("caddyfileMissingHosts = %v, want [bar.test]", missing)
	}

	none := caddyfileMissingHosts("foo.test and bar.test both here", []string{"foo.test", "bar.test"})
	if len(none) != 0 {
		t.Errorf("caddyfileMissingHosts = %v, want empty", none)
	}
}
