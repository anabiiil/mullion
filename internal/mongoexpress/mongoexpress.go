// Package mongoexpress installs mongo-express — a small Node web UI for
// MongoDB — into ~/.mullion/mongo-express and runs it as a managed node
// site, the same way phpMyAdmin is bundled for MySQL.
//
// mongo-express itself ships no way to pass its Mongo connection info,
// admin flag or session secrets on the command line; it only reads them
// from environment variables (ME_CONFIG_*) at process start. So instead
// of vendoring or patching it, Install wraps it in a tiny throwaway npm
// project — package.json depends on the pinned mongo-express version,
// and start.js sets those environment variables (with random per-install
// secrets) before importing mongo-express's own bin entry, which then
// boots its Express server exactly as `npx mongo-express` would.
package mongoexpress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"pm/internal/mongodb"
	"pm/internal/nodever"
	"pm/internal/pmdir"
	"pm/internal/proc"
)

// Version is the mongo-express release pinned in the wrapper's
// package.json.
//
// It is NOT npm's "latest" dist-tag (currently 1.1.0-rc-4). Every 1.x
// release from 1.0.1 onward — including that rc — ships a broken npm
// tarball: mongo-express moved its UI assets to a webpack build, but
// the published package omits the built output (public/build-assets.json
// and the bundled JS), which its own middleware requires at startup.
// Fetching them means running `npm run build` with the full
// devDependencies tree (webpack, babel, cypress, mongodb-memory-server,
// ...) — the opposite of "a tiny wrapper project" this package aims
// for, and confirmed broken again as recently as 1.1.0-rc-4 (verified
// by installing it and inspecting node_modules/mongo-express directly).
//
// 1.0.0 is the last release published with its build output already
// included, so `npm install` alone produces a runnable app. Pinned
// (rather than "latest") so every machine installs the exact same,
// verified-working build.
const Version = "1.0.0"

// Dir is where the wrapper project (package.json, start.js, and once
// installed, node_modules) lives.
func Dir(paths pmdir.Paths) string { return filepath.Join(paths.Home, "mongo-express") }

func nodeModulesEntry(paths pmdir.Paths) string {
	return filepath.Join(Dir(paths), "node_modules", "mongo-express", "app.js")
}

// Installed reports whether mongo-express's dependencies have already
// been fetched.
func Installed(paths pmdir.Paths) bool {
	_, err := os.Stat(nodeModulesEntry(paths))
	return err == nil
}

// Install writes the wrapper project (package.json + start.js, with
// fresh random session/cookie secrets) and fetches mongo-express with
// the given Node version directory (as returned by
// nodever/app.NodeVersionDirFor). No-op when already installed — call
// Remove first to force a clean reinstall (e.g. to rotate secrets or
// pick up a new pinned Version).
func Install(ctx context.Context, paths pmdir.Paths, nodeDir string) error {
	if Installed(paths) {
		return nil
	}
	dir := Dir(paths)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	pkgJSON, err := renderPackageJSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkgJSON, 0o644); err != nil {
		return err
	}

	cookieSecret, err := randomSecret()
	if err != nil {
		return err
	}
	sessionSecret, err := randomSecret()
	if err != nil {
		return err
	}
	startJS := renderStartJS(mongodb.ConnectionURI(), cookieSecret, sessionSecret)
	if err := os.WriteFile(filepath.Join(dir, "start.js"), []byte(startJS), 0o644); err != nil {
		return err
	}

	fmt.Println("Installing mongo-express...")
	cmd := exec.CommandContext(ctx, nodever.Tool(nodeDir, "npm"),
		"install", "--omit=dev", "--no-audit", "--no-fund")
	proc.HideConsole(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+nodever.BinDir(nodeDir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm install failed in %s: %w", dir, err)
	}
	return nil
}

// Remove deletes the wrapper project (package.json, start.js,
// node_modules) entirely.
func Remove(paths pmdir.Paths) error {
	return os.RemoveAll(Dir(paths))
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type wrapperPackageJSON struct {
	Name         string            `json:"name"`
	Private      bool              `json:"private"`
	Type         string            `json:"type"`
	Scripts      map[string]string `json:"scripts"`
	Dependencies map[string]string `json:"dependencies"`
}

// renderPackageJSON builds the wrapper project's package.json. "type":
// "module" is required (not just convention) because mongo-express
// itself is ESM-only, and start.js uses top-level await to boot it.
func renderPackageJSON() ([]byte, error) {
	pkg := wrapperPackageJSON{
		Name:         "mullion-mongo-express",
		Private:      true,
		Type:         "module",
		Scripts:      map[string]string{"start": "node start.js"},
		Dependencies: map[string]string{"mongo-express": Version},
	}
	data, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// startJSTemplate boots mongo-express with mullion's defaults applied
// BEFORE its app entry is imported (mongo-express reads its config from
// process.env at import time, so the order matters). Every setDefault
// only fills in a value that isn't already set, so an operator can still
// override anything via the environment.
//
// Version 1.0.0 (see the Version doc comment) has no single "disable
// basic auth" switch — later releases added ME_CONFIG_BASICAUTH /
// ME_CONFIG_BASICAUTH_ENABLED, but 1.0.0 predates both. Its config
// instead derives useBasicAuth from whether ME_CONFIG_BASICAUTH_USERNAME
// is set to the empty string, so that's what's defaulted here.
const startJSTemplate = `// Generated by mullion. Deleting this file (and node_modules) and
// re-running "mullion mongo-express" regenerates it with fresh secrets.
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

function setDefault(name, value) {
  if (process.env[name] === undefined) process.env[name] = value;
}

// No-auth local MongoDB (mullion's internal/mongodb, 127.0.0.1 only).
setDefault('ME_CONFIG_MONGODB_URL', %q);
setDefault('ME_CONFIG_MONGODB_ENABLE_ADMIN', 'true');

// mongo-express 1.0.0 turns basic auth OFF only when this is exactly the
// empty string (unset defaults it ON, with admin/pass credentials) — set
// it explicitly rather than leaving it unset.
setDefault('ME_CONFIG_BASICAUTH_USERNAME', '');

// Random per-install secrets for mongo-express's cookie/session signing.
setDefault('ME_CONFIG_SITE_COOKIESECRET', %q);
setDefault('ME_CONFIG_SITE_SESSIONSECRET', %q);

// mongo-express binds to VCAP_APP_HOST (falling back to "localhost")
// rather than PORT's host part — set it explicitly so it listens on
// 127.0.0.1 the way mullion's dev-server proxy expects.
setDefault('VCAP_APP_HOST', '127.0.0.1');

// PORT is provided by mullion's devserver at process start; mongo-express
// falls back to 8081 on its own if it's ever missing.

const appEntry = path.join(__dirname, 'node_modules', 'mongo-express', 'app.js');
await import(pathToFileURL(appEntry).href);
`

// renderStartJS renders start.js with the given Mongo connection string
// and per-install secrets baked in as defaults.
func renderStartJS(mongoURI, cookieSecret, sessionSecret string) string {
	return fmt.Sprintf(startJSTemplate, mongoURI, cookieSecret, sessionSecret)
}
