package phpver

import (
	"os"
	"path/filepath"
	"testing"

	"pm/internal/pmdir"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func TestSetIniDirectiveReplacesExistingLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(path, []byte("display_errors=On\nopcache.enable=1\nmemory_limit=512M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIniDirective(path, "opcache.enable", "0"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := "display_errors=On\nopcache.enable=0\nmemory_limit=512M\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetIniDirectiveHandlesSpacesAroundEquals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(path, []byte("apc.enabled = 0\nmemory_limit=512M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIniDirective(path, "apc.enabled", "1"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := "apc.enabled=1\nmemory_limit=512M\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetIniDirectiveIgnoresCommentedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(path, []byte(";opcache.enable=1\nmemory_limit=512M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIniDirective(path, "opcache.enable", "0"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := ";opcache.enable=1\nmemory_limit=512M\nopcache.enable=0\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetIniDirectiveAppendsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(path, []byte("memory_limit=512M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIniDirective(path, "apc.enabled", "1"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := "memory_limit=512M\napc.enabled=1\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetIniDirectiveLeavesOtherLinesUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php.ini")
	original := "; a comment\ndisplay_errors=On\nopcache.enable=1\ncgi.fix_pathinfo=1\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setIniDirective(path, "opcache.enable", "0"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := "; a comment\ndisplay_errors=On\nopcache.enable=0\ncgi.fix_pathinfo=1\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSetIniDirectiveCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "php.ini")
	if err := setIniDirective(path, "opcache.enable", "0"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if got != "opcache.enable=0" {
		t.Errorf("got %q, want %q", got, "opcache.enable=0")
	}
}

// TestSetIniValidation exercises SetIni's per-kind validation without
// touching a real php binary: every case here is rejected (or, for
// date.timezone, accepted/rejected) purely by the format checks that run
// before SetIni would ever shell out to php -r.
func TestSetIniValidation(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{"unknown key rejected", "not_a_real_setting", "1", true},

		{"size accepts plain number", "memory_limit", "512", false},
		{"size accepts suffix", "upload_max_filesize", "128M", false},
		{"size accepts -1", "post_max_size", "-1", false},
		{"size rejects garbage", "memory_limit", "512MB", true},
		{"size rejects empty", "memory_limit", "", true},

		{"int accepts plain number", "max_execution_time", "120", false},
		{"int accepts -1", "max_input_time", "-1", false},
		{"int rejects suffix", "max_input_vars", "1000M", true},
		{"int rejects non-numeric", "max_execution_time", "abc", true},

		{"bool accepts 1", "display_errors", "1", false},
		{"bool accepts On", "display_errors", "On", false},
		{"bool accepts Off", "opcache.validate_timestamps", "Off", false},
		{"bool rejects garbage", "display_errors", "maybe", true},

		{"select accepts exact option", "error_reporting", "E_ALL & ~E_DEPRECATED", false},
		{"select rejects unknown option", "error_reporting", "E_ALL & ~E_NOTICE", true},
		{"select rejects lowercase variant", "error_reporting", "e_all", true},

		{"text rejects empty", "date.timezone", "", true},
		{"text rejects newline", "date.timezone", "Africa/Cairo\n", true},
		{"text rejects quote", "date.timezone", `Africa/Cairo"`, true},
		{"text rejects semicolon", "date.timezone", "Africa/Cairo;", true},
		{"text rejects too long", "date.timezone", string(make([]byte, 101)), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := pmdir.Paths{Home: t.TempDir()}
			if err := os.MkdirAll(paths.PhpVersionDir("8.4"), 0o755); err != nil {
				t.Fatal(err)
			}
			err := SetIni(paths, "8.4", tc.key, tc.value)
			if tc.wantErr && err == nil {
				t.Fatalf("SetIni(%q, %q) = nil error, want an error", tc.key, tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("SetIni(%q, %q) = %v, want no error", tc.key, tc.value, err)
			}
		})
	}
}
