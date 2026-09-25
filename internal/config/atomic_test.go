package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteJSONAtomic: readers racing a writer must never see an empty
// or half-written file (the control panel reads config on every request
// while preference saves write it).
func TestWriteJSONAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	big := Config{TLD: "test", UI: map[string]string{"k": strings.Repeat("x", 64<<10)}}
	if err := writeJSON(path, big); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			if err := writeJSON(path, big); err != nil {
				t.Error(err)
				break
			}
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			wg.Wait()
			if left, _ := filepath.Glob(path + ".*.tmp"); len(left) > 0 {
				t.Fatalf("temp files left behind: %v", left)
			}
			return
		default:
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var c Config
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("reader saw a partial file (%d bytes): %v", len(data), err)
		}
	}
}
