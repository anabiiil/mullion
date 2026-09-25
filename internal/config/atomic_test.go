package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteJSONAtomic: readers racing a writer must never see an empty
// or half-written file (the control panel reads config on every request
// while preference saves write it). On Windows the file is briefly
// locked during the rename; readJSON rides that out with retries.
func TestWriteJSONAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	big := Config{TLD: "test", UI: map[string]string{"k": strings.Repeat("x", 64<<10)}}
	if err := writeJSON(path, big); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	werr := make(chan error, 1)
	go func() {
		defer close(werr)
		for i := 0; i < 200; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := writeJSON(path, big); err != nil {
				werr <- err
				return
			}
		}
	}()
	defer func() {
		close(stop)
		for range werr {
		}
	}()
	for {
		select {
		case err, ok := <-werr:
			if ok && err != nil {
				t.Fatalf("writer: %v", err)
			}
			if left, _ := filepath.Glob(path + ".*.tmp"); len(left) > 0 {
				t.Fatalf("temp files left behind: %v", left)
			}
			return
		default:
		}
		var c Config
		if err := readJSON(path, &c); err != nil {
			t.Fatalf("reader saw a partial or locked file: %v", err)
		}
		if c.TLD != "test" {
			t.Fatalf("reader saw an empty config")
		}
	}
}
