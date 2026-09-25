package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"pm/internal/config"
	"pm/internal/nodever"
	"pm/internal/pmdir"
)

func TestValidComposerName(t *testing.T) {
	valid := []string{"laravel/sanctum", "psr/log", "vendor-name/package_name", "a/b"}
	invalid := []string{"", "laravel", "/sanctum", "laravel/", "Laravel/Sanctum", "laravel/../etc", "laravel/pkg; rm -rf", "laravel/pkg name"}
	for _, n := range valid {
		if !validComposerName(n) {
			t.Errorf("validComposerName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if validComposerName(n) {
			t.Errorf("validComposerName(%q) = true, want false", n)
		}
	}
}

func TestValidNpmName(t *testing.T) {
	valid := []string{"dayjs", "lodash.debounce", "@scope/pkg", "@scope/pkg-name", "a", "some-package_1.0"}
	invalid := []string{"", "Dayjs", "@Scope/pkg", ".hidden", "_underscore", "pkg name", "pkg;rm -rf", "@scope/", "@/pkg"}
	for _, n := range valid {
		if !validNpmName(n) {
			t.Errorf("validNpmName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if validNpmName(n) {
			t.Errorf("validNpmName(%q) = true, want false", n)
		}
	}
}

func TestValidVersionConstraint(t *testing.T) {
	valid := []string{"", "1.2.3", "^1.2.3", "~1.0", "1.2.*", ">=1.0,<2.0", "dev-master", "1.0.0-beta.1"}
	invalid := []string{"1.2.3; rm -rf /", "1.0 && echo hi", "1.0 2.0", "`whoami`", "$(id)", "1.0\n2.0"}
	for _, v := range valid {
		if !validVersionConstraint(v) {
			t.Errorf("validVersionConstraint(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if validVersionConstraint(v) {
			t.Errorf("validVersionConstraint(%q) = true, want false", v)
		}
	}
}

func TestDetectNodePM(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, c.file), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectNodePM(dir); got != c.want {
			t.Errorf("detectNodePM with %s present = %q, want %q", c.file, got, c.want)
		}
	}
	// No lockfile at all falls back to npm.
	if got := detectNodePM(t.TempDir()); got != "npm" {
		t.Errorf("detectNodePM with no lockfile = %q, want npm", got)
	}
	// pnpm takes priority when multiple lockfiles somehow coexist.
	dir := t.TempDir()
	for _, f := range []string{"pnpm-lock.yaml", "yarn.lock", "bun.lockb"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := detectNodePM(dir); got != "pnpm" {
		t.Errorf("detectNodePM with multiple lockfiles = %q, want pnpm (highest priority)", got)
	}
}

func TestIsComposerPlatformPackage(t *testing.T) {
	platform := []string{"php", "PHP", "ext-json", "ext-mbstring", "lib-openssl", "composer-plugin-api", "composer-runtime-api"}
	real := []string{"laravel/sanctum", "psr/log", "monolog/monolog"}
	for _, n := range platform {
		if !isComposerPlatformPackage(n) {
			t.Errorf("isComposerPlatformPackage(%q) = false, want true", n)
		}
	}
	for _, n := range real {
		if isComposerPlatformPackage(n) {
			t.Errorf("isComposerPlatformPackage(%q) = true, want false", n)
		}
	}
}

func TestComposerInstalledPackages(t *testing.T) {
	dir := t.TempDir()
	composerJSON := map[string]any{
		"require": map[string]string{
			"php":             "^8.2",
			"laravel/sanctum": "^4.0",
			"ext-mbstring":    "*",
		},
		"require-dev": map[string]string{
			"phpunit/phpunit": "^11.0",
		},
	}
	writeJSON(t, filepath.Join(dir, "composer.json"), composerJSON)

	composerLock := map[string]any{
		"packages": []map[string]string{
			{"name": "laravel/sanctum", "version": "v4.0.2"},
		},
		"packages-dev": []map[string]string{
			{"name": "phpunit/phpunit", "version": "11.3.6"},
		},
	}
	writeJSON(t, filepath.Join(dir, "composer.lock"), composerLock)

	got, err := composerInstalledPackages(dir)
	if err != nil {
		t.Fatalf("composerInstalledPackages: %v", err)
	}
	// php and ext-mbstring are platform packages and must be excluded.
	if len(got) != 2 {
		t.Fatalf("got %d packages, want 2: %+v", len(got), got)
	}
	byName := map[string]InstalledPkg{}
	for _, p := range got {
		byName[p.Name] = p
	}
	sanctum, ok := byName["laravel/sanctum"]
	if !ok {
		t.Fatal("laravel/sanctum missing from results")
	}
	if sanctum.Constraint != "^4.0" || sanctum.Installed != "v4.0.2" || sanctum.Dev {
		t.Errorf("laravel/sanctum = %+v, want constraint ^4.0, installed v4.0.2, dev=false", sanctum)
	}
	phpunit, ok := byName["phpunit/phpunit"]
	if !ok {
		t.Fatal("phpunit/phpunit missing from results")
	}
	if phpunit.Constraint != "^11.0" || phpunit.Installed != "11.3.6" || !phpunit.Dev {
		t.Errorf("phpunit/phpunit = %+v, want constraint ^11.0, installed 11.3.6, dev=true", phpunit)
	}
}

func TestComposerInstalledPackagesNoManifest(t *testing.T) {
	got, err := composerInstalledPackages(t.TempDir())
	if err != nil {
		t.Fatalf("composerInstalledPackages: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for a directory with no composer.json", got)
	}
}

func TestComposerInstalledPackagesNoLock(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "composer.json"), map[string]any{
		"require": map[string]string{"laravel/sanctum": "^4.0"},
	})
	got, err := composerInstalledPackages(dir)
	if err != nil {
		t.Fatalf("composerInstalledPackages: %v", err)
	}
	if len(got) != 1 || got[0].Installed != "" {
		t.Errorf("got %+v, want one entry with empty Installed (no lock file yet)", got)
	}
}

func TestNpmInstalledPackages(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "package.json"), map[string]any{
		"dependencies":    map[string]string{"dayjs": "^1.11.0"},
		"devDependencies": map[string]string{"@scope/tool": "^2.0.0"},
	})
	writeJSON(t, filepath.Join(dir, "node_modules", "dayjs", "package.json"), map[string]any{"version": "1.11.13"})
	writeJSON(t, filepath.Join(dir, "node_modules", "@scope", "tool", "package.json"), map[string]any{"version": "2.0.1"})

	got, err := npmInstalledPackages(dir)
	if err != nil {
		t.Fatalf("npmInstalledPackages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d packages, want 2: %+v", len(got), got)
	}
	byName := map[string]InstalledPkg{}
	for _, p := range got {
		byName[p.Name] = p
	}
	dayjs, ok := byName["dayjs"]
	if !ok || dayjs.Constraint != "^1.11.0" || dayjs.Installed != "1.11.13" || dayjs.Dev {
		t.Errorf("dayjs = %+v", dayjs)
	}
	tool, ok := byName["@scope/tool"]
	if !ok || tool.Constraint != "^2.0.0" || tool.Installed != "2.0.1" || !tool.Dev {
		t.Errorf("@scope/tool = %+v", tool)
	}
}

func TestNpmInstalledPackagesNotYetInstalled(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "package.json"), map[string]any{
		"dependencies": map[string]string{"dayjs": "^1.11.0"},
	})
	got, err := npmInstalledPackages(dir)
	if err != nil {
		t.Fatalf("npmInstalledPackages: %v", err)
	}
	if len(got) != 1 || got[0].Installed != "" {
		t.Errorf("got %+v, want one entry with empty Installed (node_modules absent)", got)
	}
}

func TestInstalledPackagesUnknownSite(t *testing.T) {
	paths := pmdir.Paths{Home: filepath.Join(t.TempDir(), "mullion")}
	a := &App{Paths: paths, State: mustLoadState(t, paths), skipApply: true}
	if _, _, err := a.InstalledPackages("does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown site, got nil")
	}
}

func TestInstallPackageValidatesBeforeRunningAnything(t *testing.T) {
	paths := pmdir.Paths{Home: filepath.Join(t.TempDir(), "mullion")}
	state := mustLoadState(t, paths)
	site := config.Site{Name: "demo", Path: t.TempDir(), Kind: "php"}
	state.AddSite(site)
	a := &App{Paths: paths, State: state, skipApply: true}

	if err := a.InstallPackage(context.Background(), "demo", "composer", "not a valid name", "", false, nil); err == nil {
		t.Fatal("expected an error for an invalid Composer package name")
	}
	if err := a.InstallPackage(context.Background(), "demo", "npm", "also invalid", "", false, nil); err == nil {
		t.Fatal("expected an error for an invalid npm package name")
	}
	if err := a.InstallPackage(context.Background(), "demo", "composer", "vendor/pkg", "1.0; rm -rf /", false, nil); err == nil {
		t.Fatal("expected an error for a version with shell metacharacters")
	}
	if err := a.InstallPackage(context.Background(), "demo", "pip", "vendor/pkg", "", false, nil); err == nil {
		t.Fatal("expected an error for an unknown manager")
	}
	if err := a.InstallPackage(context.Background(), "no-such-site", "composer", "vendor/pkg", "", false, nil); err == nil {
		t.Fatal("expected an error for an unknown site")
	}
}

func TestResolveToolPrefersManagedNode(t *testing.T) {
	nodeDir := t.TempDir()
	binName := "npm"
	if runtime.GOOS == "windows" {
		binName = "npm.cmd"
	}
	binDir := nodever.BinDir(nodeDir)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, binName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool, err := resolveTool(nodeDir, "npm")
	if err != nil {
		t.Fatalf("resolveTool: %v", err)
	}
	if filepath.Dir(tool) != binDir {
		t.Errorf("resolveTool returned %q, want a path under %q", tool, binDir)
	}
}

func TestResolveToolMissingReportsClearError(t *testing.T) {
	if _, err := resolveTool(t.TempDir(), "definitely-not-a-real-tool-xyz"); err == nil {
		t.Fatal("expected an error when the tool isn't found anywhere")
	}
}

// writeJSON marshals v as JSON and writes it to path, creating parent
// directories as needed.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
