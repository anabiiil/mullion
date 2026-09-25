package phpver

import (
	"fmt"
	"os"

	"pm/internal/pmdir"
)

// Installed lists the full versions present under ~/.mullion/php, newest first.
func Installed(paths pmdir.Paths) ([]string, error) {
	entries, err := os.ReadDir(paths.PhpDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "current" {
			continue
		}
		if _, err := ParseSelector(e.Name()); err == nil {
			out = append(out, e.Name())
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// FindInstalled returns the newest installed version matching the selector.
func FindInstalled(paths pmdir.Paths, sel Selector) (string, error) {
	versions, err := Installed(paths)
	if err != nil {
		return "", err
	}
	best := ""
	for _, v := range versions {
		if sel.Matches(v) && (best == "" || Compare(v, best) > 0) {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("PHP %s is not installed (run: mullion php install %s)", sel, sel)
	}
	return best, nil
}
