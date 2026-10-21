package caddy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: runBounded's tests re-run this
// test binary as a stand-in `caddy reload` that hangs or fails.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("MULLION_CADDY_HELPER") {
	case "hang":
		time.Sleep(time.Minute)
	case "fail":
		os.Stderr.WriteString("loading new config: bad things")
		os.Exit(1)
	}
}

func helperCmd(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "MULLION_CADDY_HELPER="+mode)
	return cmd
}

// A reload that never answers (Caddy waiting on in-flight requests, a
// stuck admin API) must not hang its caller: it is killed and reported.
func TestRunBoundedTimesOut(t *testing.T) {
	start := time.Now()
	err := runBounded(helperCmd("hang"), 300*time.Millisecond, "caddy reload")
	if err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("runBounded returned after %s, want shortly after the timeout", d)
	}
}

func TestRunBoundedReportsFailureOutput(t *testing.T) {
	err := runBounded(helperCmd("fail"), 30*time.Second, "caddy reload")
	if err == nil || !strings.Contains(err.Error(), "bad things") {
		t.Fatalf("err = %v, want the command's output in the error", err)
	}
}

func TestRunBoundedSuccess(t *testing.T) {
	if err := runBounded(helperCmd(""), 30*time.Second, "caddy reload"); err != nil {
		t.Fatal(err)
	}
}
