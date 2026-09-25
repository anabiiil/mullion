//go:build darwin || linux

package pty

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readAll drains p until it closes or the deadline passes.
func readAll(t *testing.T, p *PTY, d time.Duration) string {
	t.Helper()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, p)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		p.Close()
		<-done
	}
	return buf.String()
}

func TestEcho(t *testing.T) {
	p, err := Start("/bin/echo", []string{"hi"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(t, p, 5*time.Second)
	if !strings.Contains(out, "hi") {
		t.Fatalf("output %q does not contain hi", out)
	}
	code, err := p.Wait()
	if err != nil || code != 0 {
		t.Fatalf("wait: code=%d err=%v", code, err)
	}
	p.Close()
}

func TestSizeResizeAndIsTTY(t *testing.T) {
	// stty size prints "rows cols" of the terminal it is attached to —
	// which also proves stdin is a real tty.
	p, err := Start("/bin/sh", []string{"-c", "stty size; read x; stty size"}, "", nil, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var got bytes.Buffer
	chunk := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "30 100") && time.Now().Before(deadline) {
		n, err := p.Read(chunk)
		got.Write(chunk[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got.String(), "30 100") {
		t.Fatalf("initial size: got %q", got.String())
	}
	if err := p.Resize(120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}
	p.Write([]byte("\n"))
	out := readAll(t, p, 5*time.Second)
	if !strings.Contains(out, "40 120") {
		t.Fatalf("after resize: got %q", out)
	}
}

func TestCwdAndEnv(t *testing.T) {
	dir := t.TempDir()
	p, err := Start("/bin/sh", []string{"-c", `echo "T=$TERM"; sleep 2`}, dir, []string{"PATH=" + os.Getenv("PATH")}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	time.Sleep(200 * time.Millisecond)
	cwd, err := Cwd(p.Pid())
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got, _ := filepath.EvalSymlinks(cwd); got != want {
		t.Fatalf("cwd = %q, want %q", cwd, want)
	}
	chunk := make([]byte, 256)
	n, _ := p.Read(chunk)
	if !strings.Contains(string(chunk[:n]), "T=xterm-256color") {
		t.Fatalf("TERM not set: %q", chunk[:n])
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	p, err := Start("/bin/sh", []string{"-c", "sleep 30"}, "", nil, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := p.Read(make([]byte, 16))
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	p.Close()
	select {
	case <-errc:
	case <-time.After(3 * time.Second):
		t.Fatal("Read still blocked after Close")
	}
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process survived Close")
	}
}
