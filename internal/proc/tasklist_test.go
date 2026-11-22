package proc

import "testing"

func TestTasklistHasPID(t *testing.T) {
	out := []byte("\"node.exe\",\"1234\",\"Console\",\"1\",\"45,120 K\"\r\n" +
		"\"frankenphp.exe\",\"88\",\"Console\",\"1\",\"1,024 K\"\r\n")
	cases := []struct {
		pid  int
		want bool
	}{
		{1234, true},
		{88, true},
		{123, false}, // a prefix of 1234
		{234, false}, // a suffix of 1234
		{8, false},
		{1, false}, // the session number column
	}
	for _, c := range cases {
		if got := TasklistHasPID(out, c.pid); got != c.want {
			t.Errorf("TasklistHasPID(%d) = %v, want %v", c.pid, got, c.want)
		}
	}
	// No match: tasklist prints an INFO line, not CSV.
	if TasklistHasPID([]byte("INFO: No tasks are running which match the specified criteria.\r\n"), 123) {
		t.Error("INFO line matched")
	}
	if TasklistHasPID(nil, 1) {
		t.Error("empty output matched")
	}
}
