package mongodb

import (
	"encoding/json"
	"testing"
)

// A trimmed shape of https://downloads.mongodb.org/tools/db/full.json —
// enough entries to check that parseToolsIndex picks the right
// platform's archive and ignores everything else (other platforms,
// pre-release versions, entries with no archive).
const toolsIndexFixture = `{
  "versions": [
    {
      "version": "100.18.0",
      "downloads": [
        {"name": "macos", "arch": "arm64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-macos-arm64-100.18.0.zip", "sha256": "aaa"}},
        {"name": "windows", "arch": "x86_64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-windows-x86_64-100.18.0.zip", "sha256": "bbb"}}
      ]
    },
    {
      "version": "100.19.0",
      "downloads": [
        {"name": "macos", "arch": "arm64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-macos-arm64-100.19.0.zip", "sha256": "ccc"}},
        {"name": "macos", "arch": "x86_64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-macos-x86_64-100.19.0.zip", "sha256": "ddd"}},
        {"name": "amazon2", "arch": "x86_64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-amazon2-x86_64-100.19.0.zip", "sha256": "eee"}}
      ]
    },
    {
      "version": "100.20.0-rc1",
      "downloads": [
        {"name": "macos", "arch": "arm64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-macos-arm64-100.20.0-rc1.zip", "sha256": "fff"}}
      ]
    },
    {
      "version": "100.21.0",
      "downloads": [
        {"name": "linux", "arch": "x86_64", "archive": {"url": "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-linux-x86_64-100.21.0.zip", "sha256": "ggg"}}
      ]
    }
  ]
}`

func mustParseToolsFixture(t *testing.T) toolsIndexDoc {
	t.Helper()
	var doc toolsIndexDoc
	if err := json.Unmarshal([]byte(toolsIndexFixture), &doc); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return doc
}

func TestParseToolsIndexPicksPlatformAndNewest(t *testing.T) {
	doc := mustParseToolsFixture(t)

	macArm := parseToolsIndex(doc, "macos", "arm64")
	if len(macArm) != 2 {
		t.Fatalf("macos/arm64: got %d releases, want 2 (100.20.0-rc1 is not stable): %+v", len(macArm), macArm)
	}
	if macArm[0].Version != "100.19.0" || macArm[0].URL == "" {
		t.Fatalf("macos/arm64 newest = %+v, want 100.19.0 first", macArm[0])
	}
	if macArm[1].Version != "100.18.0" {
		t.Fatalf("macos/arm64 second = %+v, want 100.18.0", macArm[1])
	}

	winX64 := parseToolsIndex(doc, "windows", "x86_64")
	if len(winX64) != 1 || winX64[0].Version != "100.18.0" {
		t.Fatalf("windows/x86_64 = %+v, want exactly 100.18.0", winX64)
	}

	// A platform absent from every version returns nothing, not a panic.
	if got := parseToolsIndex(doc, "freebsd", "x86_64"); len(got) != 0 {
		t.Fatalf("freebsd/x86_64 = %+v, want none", got)
	}
}

func TestToolsFallbackURLMatchesFastdlNaming(t *testing.T) {
	got := toolsFallbackURL("100.19.0")
	// indexTarget/indexArch are platform-specific consts (source_*.go);
	// just check the pattern rather than a hardcoded platform.
	want := "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-" + indexTarget + "-" + indexArch + "-100.19.0.zip"
	if got != want {
		t.Fatalf("toolsFallbackURL = %q, want %q", got, want)
	}
}

func TestToolsBaseName(t *testing.T) {
	if got := toolsBaseName("100.19.0"); got != "tools-100.19.0" {
		t.Fatalf("toolsBaseName = %q", got)
	}
}
