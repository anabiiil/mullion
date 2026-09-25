package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pm/internal/phpver"
	"pm/internal/proc"
	"pm/internal/workers"
)

// ProjectCommand is one runnable command the project page lists.
type ProjectCommand struct {
	// Group is where it comes from: artisan | console (Symfony) | npm
	// (package.json scripts, whatever the package manager) | composer.
	Group       string `json:"group"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Command is the full command line to run in the project folder.
	Command string `json:"command"`
}

const (
	consoleListTimeout = 8 * time.Second
	projectCmdTimeout  = 15 * time.Minute
)

// consoleCache remembers `artisan list` / `bin/console list` per project
// until composer.lock (or composer.json) changes — booting a framework
// takes a noticeable moment.
var consoleCache = struct {
	sync.Mutex
	m map[string]consoleCacheEntry
}{m: map[string]consoleCacheEntry{}}

type consoleCacheEntry struct {
	key  string
	cmds []ProjectCommand
}

// ProjectCommands lists what a site's project can run: artisan (Laravel)
// or bin/console (Symfony) commands, package.json scripts and
// composer.json scripts. A failing source (say, an app that can't boot
// to list its artisan commands) is reported in err while the rest are
// still returned — cmds is valid even when err != nil.
func (a *App) ProjectCommands(ctx context.Context, siteName string) ([]ProjectCommand, error) {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return nil, err
	}
	dir := site.Path
	var out []ProjectCommand
	var errs []error

	console := func(script, group, prefix string) {
		cmds, err := a.consoleCommands(ctx, siteName, dir, script, group, prefix)
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, cmds...)
	}
	if fileExists(filepath.Join(dir, "artisan")) {
		console("artisan", "artisan", "php artisan")
	} else if fileExists(filepath.Join(dir, "bin", "console")) {
		console(filepath.Join("bin", "console"), "console", "php bin/console")
	}

	if scripts := readPackageScripts(dir); len(scripts) > 0 {
		pm := packageManager(dir)
		for _, name := range sortedKeys(scripts) {
			out = append(out, ProjectCommand{
				Group: "npm", Name: name, Description: scripts[name],
				Command: pm + " run " + name,
			})
		}
	}

	if c := readComposer(dir); len(c.scripts) > 0 {
		for _, name := range sortedKeys(c.scripts) {
			// Event hooks (post-autoload-dump, post-create-project-cmd…)
			// run as part of composer operations, not on their own.
			if strings.HasPrefix(name, "pre-") || strings.HasPrefix(name, "post-") {
				continue
			}
			out = append(out, ProjectCommand{
				Group: "composer", Name: name, Description: c.scripts[name],
				Command: "composer run-script " + name,
			})
		}
	}
	return out, errors.Join(errs...)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// phpExeFor is the site's php binary ("php" from PATH as a fallback).
func (a *App) phpExeFor(siteName string) string {
	site := a.State.FindSite(siteName)
	if site == nil {
		return "php"
	}
	if v := a.SiteVersion(*site); v != "" {
		exe := filepath.Join(a.Paths.PhpVersionDir(v), phpver.PhpExeName)
		if fileExists(exe) {
			return exe
		}
	}
	return "php"
}

// consoleCommands runs `php <script> list --format=json` (cached).
func (a *App) consoleCommands(ctx context.Context, siteName, dir, script, group, prefix string) ([]ProjectCommand, error) {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return nil, err
	}
	php := a.phpExeFor(siteName)
	key := php + "|" + script + "|" + cacheStamp(dir)
	consoleCache.Lock()
	if e, ok := consoleCache.m[dir]; ok && e.key == key {
		consoleCache.Unlock()
		return e.cmds, nil
	}
	consoleCache.Unlock()

	ctx, cancel := context.WithTimeout(ctx, consoleListTimeout)
	defer cancel()
	cmd := proc.Quiet(php, script, "list", "--format=json", "--no-interaction")
	cmd.Dir = dir
	cmd.Env = a.SiteEnv(*site)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("listing %s commands: %w", group, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		workers.KillTree(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		<-done
		return nil, fmt.Errorf("listing %s commands timed out after %s", group, consoleListTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = lastLines(stdout.String(), 5)
		}
		return nil, fmt.Errorf("`%s list` failed: %v: %s", prefix, err, msg)
	}
	cmds, err := parseConsoleList(stdout.Bytes(), group, prefix)
	if err != nil {
		return nil, fmt.Errorf("`%s list`: %w", prefix, err)
	}
	consoleCache.Lock()
	consoleCache.m[dir] = consoleCacheEntry{key: key, cmds: cmds}
	consoleCache.Unlock()
	return cmds, nil
}

// cacheStamp changes whenever the project's dependencies do.
func cacheStamp(dir string) string {
	for _, f := range []string{"composer.lock", "composer.json"} {
		if info, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return f + "@" + info.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

// parseConsoleList reads Symfony Console's `list --format=json` output
// (artisan uses the same format). Framework boot noise before the JSON
// (deprecation notices on stdout) is skipped; hidden commands dropped.
func parseConsoleList(data []byte, group, prefix string) ([]ProjectCommand, error) {
	i := bytes.IndexByte(data, '{')
	if i < 0 {
		return nil, fmt.Errorf("no JSON in the output")
	}
	var list struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
		} `json:"commands"`
	}
	dec := json.NewDecoder(bytes.NewReader(data[i:]))
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("parsing the command list: %w", err)
	}
	out := make([]ProjectCommand, 0, len(list.Commands))
	for _, c := range list.Commands {
		if c.Hidden || c.Name == "" || strings.HasPrefix(c.Name, "_") {
			continue
		}
		out = append(out, ProjectCommand{
			Group: group, Name: c.Name, Description: c.Description,
			Command: prefix + " " + c.Name,
		})
	}
	return out, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ProjectCommandCmd builds the shell command that runs `command` in a
// site's folder with the site's environment (SiteEnv). The caller wires
// stdio. interactive=false puts it in its own process group without a
// console window (workers.KillTree(cmd.Process.Pid) stops all of it);
// interactive=true shares the caller's terminal so Ctrl+C reaches it
// (the CLI's `mullion run`).
func (a *App) ProjectCommandCmd(siteName, command string, interactive bool) (*exec.Cmd, error) {
	site, err := a.siteOrErr(siteName)
	if err != nil {
		return nil, err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("no command to run")
	}
	cmd := workers.ShellCommand(command)
	if interactive {
		cmd = workers.InteractiveShellCommand(command)
	}
	cmd.Dir = site.Path
	cmd.Env = a.SiteEnv(*site)
	return cmd, nil
}

// outputFunc adapts the streaming callback to an io.Writer (stdout and
// stderr share one, so exec serializes the calls).
type outputFunc func([]byte)

func (f outputFunc) Write(p []byte) (int, error) {
	if f != nil {
		f(append([]byte(nil), p...))
	}
	return len(p), nil
}

// RunProjectCommand runs a command once in the site's folder with the
// site's environment, streaming combined stdout+stderr to onOutput (it
// gets its own copy of each chunk; may be nil). There is no stdin —
// interactive commands belong in the terminal. It gives up after 15
// minutes; cancelling ctx kills the whole process tree. exitCode is the
// command's own status when it ran to completion (err nil); err is set
// when it could not start, was cancelled or timed out (exitCode -1).
func (a *App) RunProjectCommand(ctx context.Context, siteName, command string, onOutput func([]byte)) (int, error) {
	cmd, err := a.ProjectCommandCmd(siteName, command, false)
	if err != nil {
		return -1, err
	}
	ctx, cancel := context.WithTimeout(ctx, projectCmdTimeout)
	defer cancel()
	w := outputFunc(onOutput)
	cmd.Stdout = w
	cmd.Stderr = w
	// A background grandchild holding the pipe open must not hang us
	// after the command itself exits.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("starting `%s`: %w", command, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		workers.KillTree(cmd.Process.Pid)
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return -1, fmt.Errorf("`%s` did not finish within %s and was stopped", command, projectCmdTimeout)
		}
		return -1, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
	}
	return -1, err
}
