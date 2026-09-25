package postgres

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"pm/internal/download"
	"pm/internal/term"
)

// EDB's zips bundle pgAdmin 4 and StackBuilder next to the server:
// ~440 MB (macOS) / ~340 MB (Windows) of which the server, client tools
// and libraries are only ~135 MB / ~55 MB. The zip's central directory
// says where each entry lives, so fetchZip downloads just the byte
// ranges holding the entries we keep — a handful of HTTP range requests
// — and extracts them from that sparse copy. If the server won't do
// ranges (or anything about the archive looks unusual) it falls back to
// downloading the whole file.
//
// Keep the request count small: EDB's CDN (CloudFront) rate-limits and
// temporarily blocks clients that fire hundreds of small range requests.

// wantEntry filters the zip down to the database server: everything
// under pgsql/ except pgAdmin 4, StackBuilder (and its wxWidgets DLLs
// in bin/ on Windows) and the HTML/man docs.
func wantEntry(name string) bool {
	rest := strings.TrimPrefix(name, "pgsql/")
	parts := strings.Split(rest, "/")
	first := strings.ToLower(parts[0])
	switch {
	case strings.HasPrefix(first, "pgadmin"), strings.HasPrefix(first, "stackbuilder"), first == "doc":
		return false
	}
	if first == "bin" && len(parts) > 1 {
		b := strings.ToLower(parts[1])
		if strings.HasPrefix(b, "stackbuilder") || strings.HasPrefix(b, "wx") {
			return false
		}
	}
	return true
}

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// errNoPartial means the selective download can't be used for this
// archive; the caller falls back to a full download.
var errNoPartial = errors.New("selective download unavailable")

// fetchZip extracts the entries of the remote zip at url that pass
// want into dest. HTTP errors mention "HTTP <code>" so the caller can
// tell a missing file (try the next candidate) from a real failure.
func fetchZip(ctx context.Context, url, dest, tmpDir string, want func(string) bool) error {
	err := fetchZipPartial(ctx, url, dest, want)
	if err == nil || !errors.Is(err, errNoPartial) {
		return err
	}
	archivePath := filepath.Join(tmpDir, filepath.Base(url))
	if err := download.ToFile(ctx, url, archivePath); err != nil {
		return err
	}
	defer os.Remove(archivePath)
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", filepath.Base(archivePath), err)
	}
	defer zr.Close()
	return extractEntries(&zr.Reader, dest, want)
}

// cdEntry is what we need from one central-directory record.
type cdEntry struct {
	name   string
	offset int64 // of the local file header
}

// span is one contiguous byte range of the remote file kept locally.
type span struct {
	off, end int64 // [off, end) in the remote file
	local    int64 // where it starts in the local sparse file
}

// planSpans returns the byte ranges covering every wanted entry. Each
// entry runs from its local header to the next entry's header (or the
// central directory), which also covers any data descriptor. Ranges
// closer than maxGap are merged — fewer requests beat a few extra MB.
func planSpans(entries []cdEntry, cdOffset int64, want func(string) bool, maxGap int64) []span {
	sorted := append([]cdEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].offset < sorted[j].offset })
	var spans []span
	for i, e := range sorted {
		if !want(e.name) {
			continue
		}
		end := cdOffset
		if i+1 < len(sorted) {
			end = sorted[i+1].offset
		}
		if n := len(spans); n > 0 && e.offset-spans[n-1].end <= maxGap {
			if end > spans[n-1].end {
				spans[n-1].end = end
			}
			continue
		}
		spans = append(spans, span{off: e.offset, end: end})
	}
	return spans
}

// parseCentralDirectory reads the records we need from a raw central
// directory (handling the zip64 extra field for large offsets).
func parseCentralDirectory(cd []byte, count int) ([]cdEntry, error) {
	var out []cdEntry
	for p := 0; len(out) < count; {
		if p+46 > len(cd) || binary.LittleEndian.Uint32(cd[p:]) != 0x02014b50 {
			return nil, fmt.Errorf("bad central directory record at %d", p)
		}
		h := cd[p:]
		usize := uint64(binary.LittleEndian.Uint32(h[24:]))
		csize := uint64(binary.LittleEndian.Uint32(h[20:]))
		nlen := int(binary.LittleEndian.Uint16(h[28:]))
		xlen := int(binary.LittleEndian.Uint16(h[30:]))
		clen := int(binary.LittleEndian.Uint16(h[32:]))
		off := uint64(binary.LittleEndian.Uint32(h[42:]))
		if 46+nlen+xlen+clen > len(h) {
			return nil, fmt.Errorf("truncated central directory")
		}
		name := string(h[46 : 46+nlen])
		extra := h[46+nlen : 46+nlen+xlen]
		for len(extra) >= 4 {
			id := binary.LittleEndian.Uint16(extra)
			size := int(binary.LittleEndian.Uint16(extra[2:]))
			if 4+size > len(extra) {
				break
			}
			if id == 0x0001 { // zip64: only the saturated fields are present, in order
				d := extra[4 : 4+size]
				for _, field := range []*uint64{&usize, &csize, &off} {
					if *field == 0xffffffff && len(d) >= 8 {
						*field = binary.LittleEndian.Uint64(d)
						d = d[8:]
					}
				}
			}
			extra = extra[4+size:]
		}
		out = append(out, cdEntry{name: name, offset: int64(off)})
		p += 46 + nlen + xlen + clen
	}
	return out, nil
}

// locateCentralDirectory finds the central directory's offset, size and
// entry count from the archive's tail (which starts at tailOff).
func locateCentralDirectory(tail []byte, tailOff int64) (offset, size int64, count int, err error) {
	i := len(tail) - 22
	for ; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 {
			break
		}
	}
	if i < 0 {
		return 0, 0, 0, fmt.Errorf("no end-of-central-directory record")
	}
	eocd := tail[i:]
	count = int(binary.LittleEndian.Uint16(eocd[10:]))
	size = int64(binary.LittleEndian.Uint32(eocd[12:]))
	offset = int64(binary.LittleEndian.Uint32(eocd[16:]))
	if count == 0xffff || size == 0xffffffff || offset == 0xffffffff {
		// zip64: the locator sits right before the EOCD record.
		loc := i - 20
		if loc < 0 || binary.LittleEndian.Uint32(tail[loc:]) != 0x07064b50 {
			return 0, 0, 0, fmt.Errorf("zip64 locator missing")
		}
		recOff := int64(binary.LittleEndian.Uint64(tail[loc+8:])) - tailOff
		if recOff < 0 || recOff+56 > int64(len(tail)) || binary.LittleEndian.Uint32(tail[recOff:]) != 0x06064b50 {
			return 0, 0, 0, fmt.Errorf("zip64 end record outside the fetched tail")
		}
		rec := tail[recOff:]
		count = int(binary.LittleEndian.Uint64(rec[32:]))
		size = int64(binary.LittleEndian.Uint64(rec[40:]))
		offset = int64(binary.LittleEndian.Uint64(rec[48:]))
	}
	return offset, size, count, nil
}

// getRange fetches [off, end) of url (end < 0: the last -off bytes) and
// returns the body plus the file's total size from Content-Range. A
// server that ignores the Range header yields errNoPartial.
func getRange(ctx context.Context, url string, off, end int64) (io.ReadCloser, int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	if end < 0 {
		req.Header.Set("Range", "bytes=-"+strconv.FormatInt(off, 10))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end-1))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		resp.Body.Close()
		return nil, 0, 0, fmt.Errorf("%w: server ignored the Range header", errNoPartial)
	default:
		resp.Body.Close()
		return nil, 0, 0, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	// Content-Range: bytes <start>-<end>/<total>
	var start, last, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &last, &total); err != nil {
		resp.Body.Close()
		return nil, 0, 0, fmt.Errorf("%w: unexpected Content-Range %q", errNoPartial, resp.Header.Get("Content-Range"))
	}
	if end >= 0 && (start != off || last != end-1) {
		resp.Body.Close()
		return nil, 0, 0, fmt.Errorf("%w: server returned a different range", errNoPartial)
	}
	return resp.Body, start, total, nil
}

// maxSpans bounds the range requests per download (see the CDN note).
const maxSpans = 12

// tailBytes is the first request: the archive's last bytes, which hold
// the central directory (~2.5 MB for EDB's 20k+ entries).
var tailBytes int64 = 4 << 20

func fetchZipPartial(ctx context.Context, url, dest string, want func(string) bool) error {
	// 1. The tail: end-of-central-directory, usually the whole directory.
	body, tailOff, total, err := getRange(ctx, url, tailBytes, -1)
	if err != nil {
		return err
	}
	tail, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return err
	}
	cdOff, cdSize, count, err := locateCentralDirectory(tail, tailOff)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoPartial, err)
	}
	if cdOff < tailOff { // directory bigger than the tail: fetch the rest
		body, _, _, err := getRange(ctx, url, cdOff, tailOff)
		if err != nil {
			return err
		}
		head, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			return err
		}
		tail = append(head, tail...)
		tailOff = cdOff
	}
	if cdOff+cdSize > total {
		return fmt.Errorf("%w: central directory out of bounds", errNoPartial)
	}
	entries, err := parseCentralDirectory(tail[cdOff-tailOff:cdOff-tailOff+cdSize], count)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoPartial, err)
	}

	// 2. The ranges holding the entries we keep.
	spans := planSpans(entries, cdOff, want, 4<<20)
	if len(spans) == 0 || len(spans) > maxSpans {
		return fmt.Errorf("%w: %d ranges", errNoPartial, len(spans))
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	sparse, err := os.CreateTemp(filepath.Dir(dest), ".pg-sparse-*")
	if err != nil {
		return err
	}
	defer func() {
		sparse.Close()
		os.Remove(sparse.Name())
	}()
	// Whatever overlaps the tail is already in memory.
	var need int64
	for i := range spans {
		spans[i].end = min(spans[i].end, tailOff)
		if spans[i].end > spans[i].off {
			need += spans[i].end - spans[i].off
		}
	}
	prog := &progress{label: filepath.Base(url), total: need}
	var local int64
	for i := range spans {
		s := &spans[i]
		if s.end <= s.off {
			continue
		}
		body, _, _, err := getRange(ctx, url, s.off, s.end)
		if err != nil {
			prog.finish(err)
			return err
		}
		s.local = local
		n, err := io.Copy(io.MultiWriter(sparse, prog), body)
		body.Close()
		if err == nil && n != s.end-s.off {
			err = fmt.Errorf("short read: %d of %d bytes", n, s.end-s.off)
		}
		if err != nil {
			prog.finish(err)
			return fmt.Errorf("downloading %s: %w", filepath.Base(url), err)
		}
		local += n
	}
	prog.finish(nil)
	// The tail is a span too (the directory and EOCD live there).
	if _, err := sparse.Write(tail); err != nil {
		return err
	}
	spans = append(spans, span{off: tailOff, end: total, local: local})

	// 3. Extract through a ReaderAt that maps remote offsets to the
	// sparse copy; archive/zip verifies each entry's CRC-32.
	ra := &sparseReaderAt{f: sparse, spans: spans}
	zr, err := zip.NewReader(ra, total)
	if err != nil {
		return fmt.Errorf("reading %s: %w", filepath.Base(url), err)
	}
	return extractEntries(zr, dest, want)
}

// sparseReaderAt serves reads of the remote file from the locally kept
// spans; reads touching anything else fail.
type sparseReaderAt struct {
	f     *os.File
	spans []span
}

func (r *sparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	for _, s := range r.spans {
		if off < s.off || off >= s.end {
			continue
		}
		n := int64(len(p))
		var err error
		if off+n > s.end {
			n = s.end - off
			err = io.EOF
		}
		m, rerr := r.f.ReadAt(p[:n], s.local+off-s.off)
		if rerr != nil {
			return m, rerr
		}
		return m, err
	}
	return 0, fmt.Errorf("offset %d was not downloaded", off)
}

// extractEntries unpacks the wanted entries of zr into dest, keeping
// file modes and symlinks (archive.ExtractZip flattens both).
func extractEntries(zr *zip.Reader, dest string, want func(string) bool) error {
	cleanDest := filepath.Clean(dest) + string(os.PathSeparator)
	for _, f := range zr.File {
		if !want(f.Name) {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, cleanDest) {
			return fmt.Errorf("zip entry escapes destination: %s", f.Name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case mode&os.ModeSymlink != 0:
			if runtime.GOOS == "windows" {
				continue // none in the Windows zip; symlinks need privileges there
			}
			if err := extractSymlink(f, target); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		perm := mode.Perm()
		if perm == 0 {
			perm = 0o644
		}
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o200)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return nil
}

func extractSymlink(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	link, err := io.ReadAll(io.LimitReader(rc, 4096))
	rc.Close()
	if err != nil {
		return err
	}
	if filepath.IsAbs(string(link)) {
		return fmt.Errorf("zip entry %s links outside the archive", f.Name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	os.Remove(target)
	return os.Symlink(string(link), target)
}

// progress mirrors internal/download's progress line for the ranged
// downloads (whose total is the kept bytes, not the file size).
type progress struct {
	label    string
	total    int64
	written  int64
	lastDraw time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	if time.Since(p.lastDraw) > 200*time.Millisecond {
		p.lastDraw = time.Now()
		pct := 0
		if p.total > 0 {
			pct = int(p.written * 100 / p.total)
		}
		const width = 28
		filled := pct * width / 100
		bar := term.Cyan(strings.Repeat("━", filled)) + term.Dim(strings.Repeat("─", width-filled))
		fmt.Printf("\r  %s %s %3d%%  %.1f / %.1f MB%s", p.label, bar, pct,
			float64(p.written)/1e6, float64(p.total)/1e6, term.ClearLine())
	}
	return len(b), nil
}

func (p *progress) finish(err error) {
	if err != nil {
		fmt.Printf("\r  %s %s  failed after %.1f MB: %v%s\n", term.Red("✗"), p.label,
			float64(p.written)/1e6, err, term.ClearLine())
		return
	}
	fmt.Printf("\r  %s %s  %.1f MB%s\n", term.Green("✓"), p.label,
		float64(p.written)/1e6, term.ClearLine())
}
