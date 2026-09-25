//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisableShadowEntriesNvm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	zshrc := filepath.Join(home, ".zshrc")
	os.WriteFile(zshrc, []byte(`# my stuff
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"
alias ll='ls -la'
`), 0o644)

	disabled, err := DisableShadowEntries(filepath.Join(home, ".nvm/versions/node/v22.22.2/bin/node"))
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 2 {
		t.Fatalf("expected the 2 nvm lines disabled, got %v", disabled)
	}
	data, _ := os.ReadFile(zshrc)
	out := string(data)
	if !strings.Contains(out, "# disabled by mullion") ||
		strings.Contains(out, "\nexport NVM_DIR") ||
		!strings.Contains(out, "# my stuff") || !strings.Contains(out, "alias ll") {
		t.Fatalf("unexpected result:\n%s", out)
	}

	// A shared bin dir must never be touched.
	disabled, err = DisableShadowEntries("/opt/homebrew/bin/node")
	if err != nil || len(disabled) != 0 {
		t.Fatalf("shared dir must be untouched: %v %v", disabled, err)
	}
}
