// Package hosts maintains a managed block of entries inside the system
// hosts file. Only lines between the begin/end markers are ever touched.
package hosts

import (
	"fmt"
	"os"
	"strings"
)

const (
	beginMarker = "# >>> managed by mullion — do not edit this block"
	endMarker   = "# <<< managed by mullion"
)

// Render produces the new hosts file content with the managed block
// replaced by one entry per hostname (or removed entirely when empty).
func Render(current string, hostnames []string) string {
	lines := strings.Split(current, "\n")
	var kept []string
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == beginMarker:
			inBlock = true
		case trimmed == endMarker:
			inBlock = false
		case !inBlock:
			kept = append(kept, line)
		}
	}
	// Drop trailing blank lines so repeated syncs don't grow the file.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	out := strings.Join(kept, "\n")
	if len(hostnames) > 0 {
		var b strings.Builder
		b.WriteString(out)
		if out != "" {
			b.WriteString("\n")
		}
		b.WriteString("\n" + beginMarker + "\n")
		for _, h := range hostnames {
			b.WriteString(fmt.Sprintf("127.0.0.1 %s\n::1 %s\n", h, h))
		}
		b.WriteString(endMarker + "\n")
		return b.String()
	}
	if out != "" {
		out += "\n"
	}
	return out
}

// Sync rewrites the managed block to exactly match hostnames, elevating
// on Windows if the file is not writable directly.
func Sync(hostnames []string) error {
	path := filePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading hosts file %s: %w", path, err)
	}
	current := strings.ReplaceAll(string(data), "\r\n", "\n")
	next := Render(current, hostnames)
	if next == current {
		return nil
	}
	next = toNative(next)
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		if os.IsPermission(err) {
			return writeElevated(path, next)
		}
		return err
	}
	return nil
}
