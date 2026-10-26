//go:build windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shims must name mullion.exe by absolute path: npm runs them as a
// quoted bare name, and cmd.exe then expands %~dp0 to the current
// folder (the project), not the shim's.
func TestNodeShimsUseAbsoluteMullionPath(t *testing.T) {
	bin := t.TempDir()
	if err := WriteNodeShims(bin); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"node", "npm", "npx", "corepack"} {
		data, err := os.ReadFile(filepath.Join(bin, tool+".cmd"))
		if err != nil {
			t.Fatal(err)
		}
		s := string(data)
		if strings.Contains(s, "%~dp0") {
			t.Errorf("%s.cmd still uses %%~dp0:\n%s", tool, s)
		}
		if !strings.Contains(s, `"`+filepath.Join(bin, "mullion.exe")+`"`) {
			t.Errorf("%s.cmd doesn't name mullion.exe by absolute path:\n%s", tool, s)
		}
	}
}
