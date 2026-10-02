package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"unicode/utf16"

	"pm/internal/version"
)

// TestWindowsResourcesMatchVersion catches a stale rsrc_windows_amd64.syso:
// the 2.0.0 exe shipped saying 1.1.0 in Explorer's Properties because the
// .syso wasn't rebuilt after versioninfo.json was bumped. Bump
// versioninfo.json with internal/version, then run tools/brand/build.sh.
func TestWindowsResourcesMatchVersion(t *testing.T) {
	data, err := os.ReadFile("versioninfo.json")
	if err != nil {
		t.Fatal(err)
	}
	var vi struct {
		StringFileInfo struct{ FileVersion, ProductVersion string }
	}
	if err := json.Unmarshal(data, &vi); err != nil {
		t.Fatal(err)
	}
	if vi.StringFileInfo.ProductVersion != version.Number {
		t.Fatalf("versioninfo.json ProductVersion = %q, internal/version says %q", vi.StringFileInfo.ProductVersion, version.Number)
	}

	syso, err := os.ReadFile("rsrc_windows_amd64.syso")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{vi.StringFileInfo.ProductVersion, vi.StringFileInfo.FileVersion} {
		var enc bytes.Buffer
		binary.Write(&enc, binary.LittleEndian, utf16.Encode([]rune(want+"\x00")))
		if !bytes.Contains(syso, enc.Bytes()) {
			t.Errorf("rsrc_windows_amd64.syso does not carry version %q from versioninfo.json — run tools/brand/build.sh", want)
		}
	}
}
