// This file backs the control panel's package manager: searching
// Composer/npm packages (internal/pkgsearch does the actual searching)
// and installing/removing them into a linked site, plus listing what a
// site already requires. See cmd/pkg.go for the CLI surface.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"pm/internal/composer"
	"pm/internal/config"
	"pm/internal/nodever"
	"pm/internal/phpver"
	"pm/internal/sysproc"
)

// InstalledPkg is one package a site's manifest requires, enriched with
// the version actually on disk when it can be determined.
type InstalledPkg struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint"`
	Installed  string `json:"installed,omitempty"`
	Dev        bool   `json:"dev"`
}

// packageOpTimeout bounds a single install/remove — long enough for a
// cold Composer/npm cache to warm up, short enough that a hung registry
// can't leave a control-panel request stuck forever.
const packageOpTimeout = 10 * time.Minute

// composerNameRe matches a valid Composer package name ("vendor/package").
var composerNameRe = regexp.MustCompile(`^[a-z0-9]([_.-]?[a-z0-9]+)*/[a-z0-9](([_.]?|-{0,2})[a-z0-9]+)*$`)

// npmNameRe matches a valid unscoped or scoped npm package name.
var npmNameRe = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)

// versionRe matches a version/constraint string with no whitespace or
// shell metacharacters — install args always go through argv, never a
// shell, so this is about rejecting garbage input, not injection.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9._+~^*<>=,|-]+$`)

func validComposerName(name string) bool {
	return name != "" && composerNameRe.MatchString(name)
}

// validNpmName mirrors the load-bearing bits of npm's own
// validate-npm-package-name: lowercase only, printable ASCII, no
// leading dot/underscore, optional "@scope/" prefix, max 214 chars.
func validNpmName(name string) bool {
	if name == "" || len(name) > 214 {
		return false
	}
	if strings.ToLower(name) != name {
		return false
	}
	return npmNameRe.MatchString(name)
}

// validVersionConstraint accepts "" (meaning "latest") or a
// whitespace-free constraint string.
func validVersionConstraint(v string) bool {
	if v == "" {
		return true
	}
	return versionRe.MatchString(v)
}

// InstalledPackages reads a site's Composer and npm manifests and
// returns what each requires, enriched with the installed version when
// composer.lock / node_modules says what actually landed. Either slice
// is nil (not an error) when the site has no such manifest.
func (a *App) InstalledPackages(site string) (composerPkgs []InstalledPkg, npmPkgs []InstalledPkg, err error) {
	s := a.State.FindSite(site)
	if s == nil {
		return nil, nil, fmt.Errorf("no site named %q", site)
	}
	composerPkgs, err = composerInstalledPackages(s.Path)
	if err != nil {
		return nil, nil, err
	}
	npmPkgs, err = npmInstalledPackages(s.Path)
	if err != nil {
		return nil, nil, err
	}
	return composerPkgs, npmPkgs, nil
}

// InstallPackage adds a package to a site via Composer or npm (or the
// project's actual npm-family manager — see detectNodePM), streaming
// the child process's combined output to onOutput as it runs. ctx
// cancellation (or the 10-minute internal timeout) kills the process.
func (a *App) InstallPackage(ctx context.Context, site, manager, name, version string, dev bool, onOutput func([]byte)) error {
	s := a.State.FindSite(site)
	if s == nil {
		return fmt.Errorf("no site named %q", site)
	}
	if err := validatePackageArgs(manager, name); err != nil {
		return err
	}
	if !validVersionConstraint(version) {
		return fmt.Errorf("invalid version %q", version)
	}

	ctx, cancel := context.WithTimeout(ctx, packageOpTimeout)
	defer cancel()

	var cmd *exec.Cmd
	var err error
	if manager == "composer" {
		cmd, err = a.composerCommand(ctx, *s, "require", name, version, dev)
	} else {
		cmd, err = a.npmCommand(ctx, *s, "add", name, version, dev)
	}
	if err != nil {
		return err
	}
	return runPackageCommand(cmd, onOutput)
}

// RemovePackage removes a package from a site via Composer or npm (or
// the project's actual npm-family manager), streaming output the same
// way InstallPackage does.
func (a *App) RemovePackage(ctx context.Context, site, manager, name string, onOutput func([]byte)) error {
	s := a.State.FindSite(site)
	if s == nil {
		return fmt.Errorf("no site named %q", site)
	}
	if err := validatePackageArgs(manager, name); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, packageOpTimeout)
	defer cancel()

	var cmd *exec.Cmd
	var err error
	if manager == "composer" {
		cmd, err = a.composerCommand(ctx, *s, "remove", name, "", false)
	} else {
		cmd, err = a.npmCommand(ctx, *s, "remove", name, "", false)
	}
	if err != nil {
		return err
	}
	return runPackageCommand(cmd, onOutput)
}

// validatePackageArgs checks manager and name once, shared by
// InstallPackage and RemovePackage.
func validatePackageArgs(manager, name string) error {
	switch manager {
	case "composer":
		if !validComposerName(name) {
			return fmt.Errorf("invalid Composer package name %q", name)
		}
	case "npm":
		if !validNpmName(name) {
			return fmt.Errorf("invalid npm package name %q", name)
		}
	default:
		return fmt.Errorf("unknown package manager %q (want composer or npm)", manager)
	}
	return nil
}

// composerCommand builds (but does not run) the Composer invocation for
// a site: its own resolved PHP runs composer.phar directly, the same
// way `mullion composer`'s shim does but pinned to the SITE's PHP
// rather than whatever `php/current` happens to be.
func (a *App) composerCommand(ctx context.Context, s config.Site, verb, name, version string, dev bool) (*exec.Cmd, error) {
	pharPath := composer.PharPath(a.Paths)
	if !pkgFileExists(pharPath) {
		return nil, fmt.Errorf("Composer is not installed (run: mullion composer install)")
	}
	phpVersion := a.SiteVersion(s)
	if phpVersion == "" {
		return nil, fmt.Errorf("no PHP version resolved for %s", s.Name)
	}
	phpExe := filepath.Join(a.Paths.PhpVersionDir(phpVersion), phpver.PhpExeName)
	if !pkgFileExists(phpExe) {
		return nil, fmt.Errorf("PHP %s is not installed (needed to run Composer for %s)", phpVersion, s.Name)
	}

	var args []string
	iniPath := filepath.Join(a.Paths.PhpVersionDir(phpVersion), "php.ini")
	if pkgFileExists(iniPath) {
		args = append(args, "-c", iniPath)
	}
	args = append(args, pharPath, verb)
	if verb == "require" {
		if dev {
			args = append(args, "--dev")
		}
		pkgArg := name
		if version != "" {
			pkgArg = name + ":" + version
		}
		args = append(args, pkgArg)
	} else {
		args = append(args, name)
	}
	args = append(args, "--no-interaction")

	cmd := exec.CommandContext(ctx, phpExe, args...)
	cmd.Dir = s.Path
	return cmd, nil
}

// npmCommand builds (but does not run) the install/remove invocation
// for a Node site, picking pnpm/yarn/bun/npm by whichever lockfile the
// project actually has (detectNodePM), and putting the site's managed
// Node bin dir first on PATH so a plain "npm" resolves to Mullion's.
func (a *App) npmCommand(ctx context.Context, s config.Site, verb, name, version string, dev bool) (*exec.Cmd, error) {
	nodeDir, err := a.NodeVersionDirFor(s)
	if err != nil {
		return nil, fmt.Errorf("resolving Node for %s: %w", s.Name, err)
	}

	pkgArg := name
	if version != "" {
		pkgArg += "@" + version
	}

	pm := detectNodePM(s.Path)
	var toolName string
	var args []string
	switch pm {
	case "pnpm":
		toolName = "pnpm"
		if verb == "add" {
			args = []string{"add"}
			if dev {
				args = append(args, "-D")
			}
			args = append(args, pkgArg)
		} else {
			args = []string{"remove", name}
		}
	case "yarn":
		toolName = "yarn"
		if verb == "add" {
			args = []string{"add"}
			if dev {
				args = append(args, "--dev")
			}
			args = append(args, pkgArg)
		} else {
			args = []string{"remove", name}
		}
	case "bun":
		toolName = "bun"
		if verb == "add" {
			args = []string{"add"}
			if dev {
				args = append(args, "-d")
			}
			args = append(args, pkgArg)
		} else {
			args = []string{"remove", name}
		}
	default:
		toolName = "npm"
		if verb == "add" {
			args = []string{"install"}
			if dev {
				args = append(args, "--save-dev")
			}
			args = append(args, pkgArg)
		} else {
			args = []string{"uninstall", name}
		}
	}

	tool, err := resolveTool(nodeDir, toolName)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = s.Path
	cmd.Env = append(os.Environ(), "PATH="+nodever.BinDir(nodeDir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return cmd, nil
}

// detectNodePM picks the package manager a Node project actually uses,
// by whichever lockfile is present — falling back to npm when none is.
func detectNodePM(sitePath string) string {
	switch {
	case pkgFileExists(filepath.Join(sitePath, "pnpm-lock.yaml")):
		return "pnpm"
	case pkgFileExists(filepath.Join(sitePath, "yarn.lock")):
		return "yarn"
	case pkgFileExists(filepath.Join(sitePath, "bun.lockb")), pkgFileExists(filepath.Join(sitePath, "bun.lock")):
		return "bun"
	default:
		return "npm"
	}
}

// resolveTool locates a Node-family tool: first inside the site's
// managed Node version (where npm/npx always live, and pnpm/yarn would
// too once `corepack enable` has been run there), then falling back to
// the real PATH for a globally installed pnpm/yarn/bun.
func resolveTool(nodeDir, name string) (string, error) {
	if p := nodever.Tool(nodeDir, name); pkgFileExists(p) {
		return p, nil
	}
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		candidates = []string{name + ".cmd", name + ".exe", name + ".bat"}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		for _, cand := range candidates {
			p := filepath.Join(dir, cand)
			if pkgFileExists(p) {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s is not installed — install it globally, or run `corepack enable` inside the Node version this site uses", name)
}

// callbackWriter adapts an onOutput callback to io.Writer so it can be
// wired up as a *exec.Cmd's Stdout/Stderr.
type callbackWriter struct{ fn func([]byte) }

func (w *callbackWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	w.fn(b)
	return len(p), nil
}

// runPackageCommand runs a prepared install/remove command, streaming
// its combined output and making sure a context cancellation actually
// stops the process rather than leaking it.
func runPackageCommand(cmd *exec.Cmd, onOutput func([]byte)) error {
	if onOutput != nil {
		w := &callbackWriter{fn: onOutput}
		cmd.Stdout = w
		cmd.Stderr = w
	}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			sysproc.KillWithParent(cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(cmd.Path), err)
	}
	return nil
}

// composerPlatformPackages are Composer "platform" requirements —
// php itself, extensions, the runtime APIs — that `composer require`
// can't install as a real package, so InstalledPackages leaves them out.
func isComposerPlatformPackage(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "php", "php-64bit", "hhvm":
		return true
	}
	for _, prefix := range []string{"ext-", "lib-", "composer-plugin-api", "composer-runtime-api"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// composerInstalledPackages reads composer.json's require/require-dev
// and, when composer.lock exists, the version actually locked for each.
func composerInstalledPackages(sitePath string) ([]InstalledPkg, error) {
	var doc struct {
		Require    map[string]string `json:"require"`
		RequireDev map[string]string `json:"require-dev"`
	}
	found, err := readJSONIfExists(filepath.Join(sitePath, "composer.json"), &doc)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	installed := map[string]string{}
	var lock struct {
		Packages []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
		PackagesDev []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages-dev"`
	}
	if ok, lockErr := readJSONIfExists(filepath.Join(sitePath, "composer.lock"), &lock); lockErr == nil && ok {
		for _, p := range lock.Packages {
			installed[p.Name] = p.Version
		}
		for _, p := range lock.PackagesDev {
			installed[p.Name] = p.Version
		}
	}

	var out []InstalledPkg
	for name, constraint := range doc.Require {
		if isComposerPlatformPackage(name) {
			continue
		}
		out = append(out, InstalledPkg{Name: name, Constraint: constraint, Installed: installed[name], Dev: false})
	}
	for name, constraint := range doc.RequireDev {
		if isComposerPlatformPackage(name) {
			continue
		}
		out = append(out, InstalledPkg{Name: name, Constraint: constraint, Installed: installed[name], Dev: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// npmInstalledPackages reads package.json's dependencies/devDependencies
// and, for each, the version actually unpacked under node_modules.
func npmInstalledPackages(sitePath string) ([]InstalledPkg, error) {
	var doc struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	found, err := readJSONIfExists(filepath.Join(sitePath, "package.json"), &doc)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	var out []InstalledPkg
	for name, constraint := range doc.Dependencies {
		out = append(out, InstalledPkg{Name: name, Constraint: constraint, Installed: installedNpmVersion(sitePath, name), Dev: false})
	}
	for name, constraint := range doc.DevDependencies {
		out = append(out, InstalledPkg{Name: name, Constraint: constraint, Installed: installedNpmVersion(sitePath, name), Dev: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// installedNpmVersion reads the version actually unpacked at
// node_modules/<name>/package.json ("" when it isn't installed yet).
func installedNpmVersion(sitePath, name string) string {
	parts := append([]string{sitePath, "node_modules"}, strings.Split(name, "/")...)
	parts = append(parts, "package.json")
	var pkg struct {
		Version string `json:"version"`
	}
	if ok, err := readJSONIfExists(filepath.Join(parts...), &pkg); err != nil || !ok {
		return ""
	}
	return pkg.Version
}

// readJSONIfExists decodes path into out, reporting found=false (no
// error) when the file simply doesn't exist yet — the normal case for
// a project that hasn't run install, or has no such manifest at all.
func readJSONIfExists(path string, out any) (found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	return true, nil
}

// pkgFileExists reports whether path exists and is a regular file (not
// a directory). Named to avoid colliding with the fileExists helpers
// already declared elsewhere in this package.
func pkgFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
