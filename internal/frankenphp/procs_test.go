package frankenphp

import "testing"

func TestIsFrankenName(t *testing.T) {
	cases := map[string]bool{
		"/Users/me/.mullion/frankenphp/v1.14.0/frankenphp": true,
		"frankenphp":     true,
		"frankenphp.exe": true,
		"FrankenPHP.EXE": true,
		`C:\Users\me\.mullion\frankenphp\v1.14.0\frankenphp.exe`: true,
		"/usr/sbin/sshd": false,
		"/Users/me/.mullion/frankenphp/v1.14.0/php": false, // only the base name counts
		`C:\m\frankenphp\v1.14.0\node.exe`:          false,
		"":                                          false,
	}
	for name, want := range cases {
		if got := isFrankenName(name); got != want {
			t.Errorf("isFrankenName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestTasklistImage(t *testing.T) {
	out := []byte("\"node.exe\",\"1234\",\"Console\",\"1\",\"45,120 K\"\r\n" +
		"\"frankenphp.exe\",\"88\",\"Console\",\"1\",\"1,024 K\"\r\n")
	if got := tasklistImage(out, 88); got != "frankenphp.exe" {
		t.Errorf("pid 88 = %q", got)
	}
	if got := tasklistImage(out, 123); got != "" {
		t.Errorf("pid 123 (a prefix of 1234) = %q", got)
	}
	if got := tasklistImage([]byte("INFO: No tasks are running which match the specified criteria.\r\n"), 88); got != "" {
		t.Errorf("INFO line = %q", got)
	}
}
