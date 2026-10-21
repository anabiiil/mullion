package watch

import (
	"bytes"
	"io"
	"os"
)

// Tailer follows one append-only text file by polling. It opens the
// file only while reading (so the writer can rename it for rotation —
// Windows refuses to rename a file someone holds open), notices a
// rotation (a different file now at the path) and a truncation (the
// file got shorter than what was read), and starts the new content from
// its beginning. Lines are returned without their newline; a partial
// last line waits for its newline.
type Tailer struct {
	Path string
	// Floor is where reading may start at the earliest (a clear's mark,
	// see ClearState.Offsets): Backlog doesn't look before it, and a file
	// first read from its start begins there. A file shorter than its
	// floor was truncated or replaced since, and is read normally.
	Floor   int64
	off     int64
	info    os.FileInfo
	partial []byte
	started bool
}

// maxRead bounds one poll's read (the rest comes on the next poll).
const maxRead = 4 << 20

// maxLine caps a single line (a runaway line without a newline).
const maxLine = 1 << 20

// NewTailer follows path. Nothing is read until Backlog or Poll.
func NewTailer(path string) *Tailer { return &Tailer{Path: path} }

// Backlog returns up to n complete lines from the end of the file (as
// much as fits in the last maxBytes) and positions the tailer at the
// end, so the next Poll returns only what is appended after.
func (t *Tailer) Backlog(n int, maxBytes int64) []string {
	t.started = true
	fi, err := statPinned(t.Path)
	if err != nil {
		t.off, t.info, t.Floor = 0, nil, 0 // gone since the mark: a new file will be read whole
		return nil
	}
	t.info = fi
	size := fi.Size()
	t.off = size
	floor := t.Floor
	t.Floor = 0 // positioned at the end: the mark is behind us
	if n <= 0 || size == 0 {
		return nil
	}
	// One byte before the window: whether the window starts on a line
	// boundary or mid-line, everything up to the first newline is then
	// not a whole line.
	start := size - maxBytes - 1
	if start < 0 {
		start = 0
	}
	// A clear's mark is a line boundary: start right there.
	atFloor := false
	if floor > 0 && floor <= size && start < floor {
		start, atFloor = floor, true
	}
	if start == size {
		return nil
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil
	}
	// A partial line at the end is left for Poll.
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		t.partial = append(t.partial[:0], buf[i+1:]...)
		buf = buf[:i]
	} else {
		t.partial = append(t.partial[:0], buf...)
		return nil
	}
	// Skip the (probably cut) first line when we didn't start at 0.
	if start > 0 && !atFloor {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			return nil
		}
	}
	lines := splitLines(buf)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Poll returns the complete lines appended since the last call.
func (t *Tailer) Poll() []string {
	if !t.started {
		t.started = true
	}
	fi, err := statPinned(t.Path)
	if err != nil {
		// Gone (rotated away, not yet recreated): start over when it's back.
		t.off, t.info, t.partial, t.Floor = 0, nil, t.partial[:0], 0
		return nil
	}
	if t.info != nil && !os.SameFile(t.info, fi) {
		t.off, t.partial, t.Floor = 0, t.partial[:0], 0 // rotated: a new file
	}
	if fi.Size() < t.off {
		t.off, t.partial, t.Floor = 0, t.partial[:0], 0 // truncated
	}
	if t.info == nil && t.off == 0 && t.Floor > 0 {
		if t.Floor <= fi.Size() {
			t.off = t.Floor // first read: start at the clear's mark
		}
		t.Floor = 0
	}
	t.info = fi
	if fi.Size() == t.off {
		return nil
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return nil
	}
	defer f.Close()
	want := fi.Size() - t.off
	if want > maxRead {
		want = maxRead
	}
	buf := make([]byte, want)
	n, err := f.ReadAt(buf, t.off)
	if err != nil && err != io.EOF {
		return nil
	}
	buf = buf[:n]
	t.off += int64(n)
	data := append(t.partial, buf...)
	i := bytes.LastIndexByte(data, '\n')
	if i < 0 {
		if len(data) > maxLine {
			t.partial = t.partial[:0]
			return []string{string(data)}
		}
		t.partial = data
		return nil
	}
	lines := splitLines(data[:i])
	t.partial = append([]byte(nil), data[i+1:]...)
	return lines
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{'\n'})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(bytes.TrimSuffix(p, []byte{'\r'})))
	}
	return out
}

// statPinned stats path and pins the file's identity now. On Windows an
// os.Stat result loads its file ID lazily — by path, at the first
// os.SameFile call — so a FileInfo kept from before a rename would
// resolve to the NEW file at that path and rotation would go unnoticed.
// Comparing the result with itself loads (and caches) the ID while the
// path still names this file; elsewhere it is free.
func statPinned(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err == nil {
		os.SameFile(fi, fi)
	}
	return fi, err
}
