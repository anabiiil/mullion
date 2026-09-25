// Package download fetches files over HTTP with a simple progress indicator.
package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pm/internal/term"
)

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// ToFile downloads url into destPath (creating parent directories),
// writing to a temp file first so a failed download never leaves a
// truncated file behind.
func ToFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	pw := &progressWriter{total: resp.ContentLength, label: filepath.Base(destPath)}
	_, err = io.Copy(io.MultiWriter(tmp, pw), resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), destPath)
	}
	pw.finish(err)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", filepath.Base(destPath), err)
	}
	return nil
}

type progressWriter struct {
	total    int64
	written  int64
	label    string
	lastDraw time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.written += int64(len(p))
	if time.Since(w.lastDraw) > 200*time.Millisecond {
		w.lastDraw = time.Now()
		w.draw()
	}
	return len(p), nil
}

const barWidth = 28

func (w *progressWriter) draw() {
	if w.total > 0 {
		pct := int(w.written * 100 / w.total)
		filled := pct * barWidth / 100
		bar := term.Cyan(strings.Repeat("━", filled)) + term.Dim(strings.Repeat("─", barWidth-filled))
		fmt.Printf("\r  %s %s %3d%%  %.1f / %.1f MB%s", w.label, bar, pct,
			float64(w.written)/1e6, float64(w.total)/1e6, term.ClearLine())
	} else {
		fmt.Printf("\r  %s  %.1f MB%s", w.label, float64(w.written)/1e6, term.ClearLine())
	}
}

// finish closes the progress line honestly: ✓ only when the download
// actually completed — a failed transfer must never look like success.
func (w *progressWriter) finish(err error) {
	if err != nil {
		fmt.Printf("\r  %s %s  failed after %.1f MB: %v%s\n", term.Red("✗"), w.label,
			float64(w.written)/1e6, err, term.ClearLine())
		return
	}
	fmt.Printf("\r  %s %s  %.1f MB%s\n", term.Green("✓"), w.label,
		float64(w.written)/1e6, term.ClearLine())
}
