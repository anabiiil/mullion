//go:build !windows

package frankenphp

import (
	"net"
	"net/http"
	"os"
	"testing"
)

// TestMain doubles as the fake server the fake frankenphp script execs
// (FAKE_FRANKEN_SERVE=<port>): the test binary itself, so the tests
// don't depend on a python3 that a CI runner may not start in time.
func TestMain(m *testing.M) {
	if port := os.Getenv("FAKE_FRANKEN_SERVE"); port != "" {
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			os.Stderr.WriteString("fake server: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("fake server listening on 127.0.0.1:" + port + "\n")
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		}))
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err == nil {
		os.Setenv("FAKE_SERVER_BIN", exe)
	}
	os.Exit(m.Run())
}
