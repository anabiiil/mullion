//go:build darwin

package phpver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendLdapIniAppendsOnce(t *testing.T) {
	dir := t.TempDir()
	iniPath := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(iniPath, []byte("display_errors=On\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	so := filepath.Join(dir, "ext", "ldap.so")
	if err := appendLdapIni(iniPath, "8.4.23", so); err != nil {
		t.Fatalf("first append: %v", err)
	}
	first, err := os.ReadFile(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(first), "extension=\""+so+"\""); got != 1 {
		t.Fatalf("expected exactly one extension line after first append, got %d in:\n%s", got, first)
	}
	if !strings.Contains(string(first), "display_errors=On") {
		t.Fatalf("existing content was clobbered:\n%s", first)
	}

	// A second call must not duplicate the line.
	if err := appendLdapIni(iniPath, "8.4.23", so); err != nil {
		t.Fatalf("second append: %v", err)
	}
	second, err := os.ReadFile(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(second), "extension=\""+so+"\""); got != 1 {
		t.Fatalf("expected exactly one extension line after second (idempotent) append, got %d in:\n%s", got, second)
	}
}

func TestAppendLdapIniRespectsCommentedLine(t *testing.T) {
	dir := t.TempDir()
	iniPath := filepath.Join(dir, "php.ini")
	so := filepath.Join(dir, "ext", "ldap.so")
	commented := ";extension=\"" + so + "\"\n"
	if err := os.WriteFile(iniPath, []byte(commented), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := appendLdapIni(iniPath, "8.4.23", so); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, err := os.ReadFile(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, commented) {
		t.Fatalf("commented-out line should be left in place:\n%s", content)
	}
	activeCount := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "extension=\""+so+"\"" {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active (uncommented) extension line alongside the commented one, got %d in:\n%s", activeCount, content)
	}
}

func TestAppendLdapIniPreservesMode(t *testing.T) {
	dir := t.TempDir()
	iniPath := filepath.Join(dir, "php.ini")
	if err := os.WriteFile(iniPath, []byte("display_errors=On\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	so := filepath.Join(dir, "ext", "ldap.so")
	if err := appendLdapIni(iniPath, "8.4.23", so); err != nil {
		t.Fatalf("append: %v", err)
	}
	info, err := os.Stat(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed: got %v, want 0600", info.Mode().Perm())
	}
}

func TestPreflightLdapToolsFindsSDK(t *testing.T) {
	// This exercises the real toolchain on the machine running the test
	// (Command Line Tools are assumed present in CI/dev, same assumption
	// the rest of the darwin build already makes for clang/make).
	sdk, err := preflightLdapTools()
	if err != nil {
		t.Skipf("Command Line Tools not available in this environment: %v", err)
	}
	if sdk.path == "" {
		t.Fatal("expected a non-empty SDK path")
	}
	if _, err := os.Stat(filepath.Join(sdk.path, "usr", "include", "ldap.h")); err != nil {
		t.Fatalf("expected ldap.h under the resolved SDK path: %v", err)
	}
}
