//go:build windows

package sysproc

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProcessesUnderFindsSelf(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, image := filepath.Dir(exe), filepath.Base(exe)
	if !slices.Contains(ProcessesUnder(dir, image), os.Getpid()) {
		t.Fatalf("ProcessesUnder(%q, %q) misses this test process (%d)", dir, image, os.Getpid())
	}
	if !slices.Contains(ProcessesUnder(dir, ""), os.Getpid()) {
		t.Fatalf("ProcessesUnder(%q, \"\") misses this test process", dir)
	}
	if slices.Contains(ProcessesUnder(dir, "not-"+image), os.Getpid()) {
		t.Fatal("an image filter must exclude other names")
	}
	// A sibling folder that merely shares the prefix is not "under" dir.
	if slices.Contains(ProcessesUnder(dir[:len(dir)-1], ""), os.Getpid()) {
		t.Fatal("a prefix of the folder name must not match")
	}
}

func TestPortOwnerAndActive(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		ln, err := net.Listen(network, "localhost:0")
		if err != nil {
			t.Logf("%s: %v", network, err)
			continue
		}
		port := ln.Addr().(*net.TCPAddr).Port
		pid, name := PortOwner(port)
		if pid != os.Getpid() || !strings.EqualFold(name, filepath.Base(os.Args[0])) {
			t.Errorf("%s PortOwner(%d) = %d %q, want %d %q", network, port, pid, name, os.Getpid(), filepath.Base(os.Args[0]))
		}
		if PortActive(port) {
			t.Errorf("%s: no connection yet, PortActive must be false", network)
		}
		go func() {
			if c, err := ln.Accept(); err == nil {
				defer c.Close()
				c.Read(make([]byte, 1))
			}
		}()
		c, err := net.Dial(network, ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if !PortActive(port) {
			t.Errorf("%s: an open connection must make PortActive true", network)
		}
		c.Close()
		ln.Close()
	}
	if pid, _ := PortOwner(1); pid != 0 {
		t.Errorf("nothing listens on port 1, got pid %d", pid)
	}
}
