package profiler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Delete removes one report.
func Delete(dir, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("bad profile id %q", id)
	}
	var firstErr error
	for _, p := range []string{metaFile(dir, id), traceFile(dir, id)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Clear removes every report in dir (SPX's files only) and returns how
// many there were.
func Clear(dir string) (int, error) {
	metas, err := List(dir)
	if err != nil {
		return 0, err
	}
	for _, m := range metas {
		if err := Delete(dir, m.ID); err != nil {
			return 0, err
		}
	}
	// Traces whose metadata never got written (a request killed mid-way).
	orphans, _ := filepath.Glob(filepath.Join(dir, "spx-full-*.txt.gz"))
	for _, p := range orphans {
		_ = os.Remove(p)
	}
	return len(metas), nil
}

// Enforce deletes the oldest reports beyond the limits and returns how
// many it removed.
func Enforce(dir string, lim Limits) (int, error) {
	metas, err := List(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	var total int64
	for i, m := range metas {
		total += m.Bytes
		over := (lim.MaxReports > 0 && i >= lim.MaxReports) || (lim.MaxBytes > 0 && total > lim.MaxBytes && i > 0)
		if !over {
			continue
		}
		if err := Delete(dir, m.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// Usage is how many reports dir holds and their size on disk.
func Usage(dir string) (count int, bytes int64) {
	metas, _ := List(dir)
	for _, m := range metas {
		bytes += m.Bytes
	}
	return len(metas), bytes
}
