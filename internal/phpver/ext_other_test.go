//go:build !windows

package phpver

import "testing"

func TestCanonicalExtNameFoldsZendOpcache(t *testing.T) {
	if got := canonicalExtName("Zend OPcache"); got != "opcache" {
		t.Errorf("got %q, want %q", got, "opcache")
	}
	if got := canonicalExtName("apcu"); got != "apcu" {
		t.Errorf("got %q, want %q", got, "apcu")
	}
}

func TestIniBoolOn(t *testing.T) {
	cases := map[string]bool{
		"1": true, "On": true, "on": true, "true": true, "TRUE": true,
		"": false, "0": false, "Off": false, "off": false,
	}
	for in, want := range cases {
		if got := iniBoolOn(in); got != want {
			t.Errorf("iniBoolOn(%q) = %v, want %v", in, got, want)
		}
	}
}
