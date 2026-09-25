package mongodb

import (
	"archive/zip"
	"compress/flate"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pm/internal/term"
)

// httpReaderAt reads a remote file with HTTP range requests, keeping one
// window of read-ahead so archive/zip's many small reads of the central
// directory cost a handful of requests, not hundreds.
type httpReaderAt struct {
	ctx    context.Context
	client *http.Client
	url    string
	size   int64
	winOff int64
	win    []byte
}

const readAhead = 1 << 20

func openHTTPReaderAt(ctx context.Context, url string) (*httpReaderAt, error) {
	client := &http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HEAD %s: HTTP %s", url, resp.Status)
	}
	if resp.ContentLength <= 0 || !strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
		return nil, fmt.Errorf("%s does not support range requests", url)
	}
	return &httpReaderAt{ctx: ctx, client: client, url: url, size: resp.ContentLength}, nil
}

// rangeBody opens [off, off+n) of the remote file.
func (r *httpReaderAt) rangeBody(off, n int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10))
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s (range): HTTP %s", r.url, resp.Status)
	}
	return resp.Body, nil
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	if off < r.winOff || off+int64(len(p)) > r.winOff+int64(len(r.win)) {
		n := int64(len(p))
		if n < readAhead {
			n = readAhead
		}
		if off+n > r.size {
			n = r.size - off
		}
		body, err := r.rangeBody(off, n)
		if err != nil {
			return 0, err
		}
		buf, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return 0, err
		}
		r.winOff, r.win = off, buf
	}
	n := copy(p, r.win[off-r.winOff:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// extractRemoteZip pulls only the entries `want` selects out of a remote
// zip into destDir (keeping their paths): the central directory is read
// with small range requests, then each wanted entry's compressed bytes
// are streamed in one request, inflated, and checked against the CRC32
// the zip records.
func extractRemoteZip(ctx context.Context, url, destDir string, want func(name string) bool) error {
	ra, err := openHTTPReaderAt(ctx, url)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(ra, ra.size)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(url), err)
	}
	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	found := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !want(f.Name) {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, cleanDest) {
			return fmt.Errorf("zip entry escapes destination: %s", f.Name)
		}
		if err := fetchEntry(ra, f, target); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("%s holds none of the expected files", filepath.Base(url))
	}
	return nil
}

func fetchEntry(ra *httpReaderAt, f *zip.File, target string) error {
	off, err := f.DataOffset()
	if err != nil {
		return err
	}
	if f.CompressedSize64 == 0 {
		return fmt.Errorf("%s: unexpected empty entry", f.Name)
	}
	body, err := ra.rangeBody(off, int64(f.CompressedSize64))
	if err != nil {
		return err
	}
	defer body.Close()

	label := filepath.Base(target)
	pr := &countReader{r: body, total: int64(f.CompressedSize64), label: label}
	var src io.Reader
	switch f.Method {
	case zip.Store:
		src = pr
	case zip.Deflate:
		fr := flate.NewReader(pr)
		defer fr.Close()
		src = fr
	default:
		return fmt.Errorf("%s: unsupported zip compression method %d", f.Name, f.Method)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := crc32.NewIEEE()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	if err == nil && uint64(n) != f.UncompressedSize64 {
		err = fmt.Errorf("size mismatch (got %d bytes, want %d)", n, f.UncompressedSize64)
	}
	if err == nil && h.Sum32() != f.CRC32 {
		err = fmt.Errorf("checksum mismatch")
	}
	pr.finish(err)
	if err != nil {
		os.Remove(target)
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	return nil
}

// countReader draws a download progress line like internal/download's.
type countReader struct {
	r        io.Reader
	total    int64
	read     int64
	label    string
	lastDraw time.Time
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if time.Since(c.lastDraw) > 200*time.Millisecond {
		c.lastDraw = time.Now()
		pct := int64(100)
		if c.total > 0 {
			pct = c.read * 100 / c.total
		}
		fmt.Printf("\r  %s %3d%%  %.1f / %.1f MB%s", c.label, pct,
			float64(c.read)/1e6, float64(c.total)/1e6, term.ClearLine())
	}
	return n, err
}

func (c *countReader) finish(err error) {
	if err != nil {
		fmt.Printf("\r  %s %s  failed after %.1f MB: %v%s\n", term.Red("✗"), c.label,
			float64(c.read)/1e6, err, term.ClearLine())
		return
	}
	fmt.Printf("\r  %s %s  %.1f MB%s\n", term.Green("✓"), c.label, float64(c.read)/1e6, term.ClearLine())
}
