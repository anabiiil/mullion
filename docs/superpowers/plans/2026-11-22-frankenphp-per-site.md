# FrankenPHP per site — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Each PHP site in Mullion can run on FrankenPHP instead of php-fpm/php-cgi, chosen per project from the project page or the CLI, on macOS and Windows, with optional worker mode for Laravel Octane projects.

**Architecture:** FrankenPHP is downloaded from its official GitHub releases into `~/.mullion/frankenphp/<tag>/`. Each FrankenPHP site gets its own FrankenPHP process listening on `127.0.0.1:<port>`, started from a generated per-site Caddyfile (classic mode) or through `artisan octane:frankenphp` (worker mode). Mullion's main Caddy keeps TLS, hosts and access logs, and reverse-proxies the site's domain to that port. It works like node dev servers do today, but the process is always on, with no idle sleep.

**Tech Stack:** Go (module `pm`, cobra), Caddy, FrankenPHP ≥ 1.12 (native Windows since 1.12.0), vanilla JS UI in `internal/ui`.

**Spec:** this plan (the user approved "go ahead" on the approach summarised in the conversation of 2026-11-22). Decisions that were Claude's calls are marked *(call)*.

## Global Constraints

- **Platforms:** works on macOS (arm64) and Windows (x86_64).
  - Assets: `frankenphp-mac-arm64` (a static binary, about 206 MB) and `frankenphp-windows-x86_64.zip`.
  - Intel Macs have no asset. Fail there with this exact message: `FrankenPHP has no build for Intel Macs — keep this site on PHP-FPM`.
- **Verify every download** against GitHub's `digest` (`sha256:<hex>`) before installing it. The same pattern is used in `internal/redis/release.go`.
- **FrankenPHP bundles its own PHP:**
  - 1.14.x ships PHP 8.6. The bundled version is read with `<bin> -v` after install and shown in the UI.
  - The site's pinned PHP version keeps being used for its CLI: artisan, workers, composer and Explain. *(call)*
- **Sites with the default server are unchanged:** sites without `Server` produce byte-identical Caddyfiles to today. The existing caddyfile tests must pass untouched.
- **Windows processes:** use `proc.DetachHiddenConsole` for long-running processes and `proc.Quiet` for short ones. No visible console windows.
- **Tests never touch the real environment:** no real `~/.mullion`, hosts file, or projects. Network tests point at `httptest` servers.
- **Never** install anything system-wide (brew, scoop, …).
- **Commits:** plain messages, no Claude attribution, no push.

## Review Focus

1. **Laravel sees the real host and scheme.**
   - `request()->getHost()` must be `blog.test`, not `127.0.0.1:44001`.
   - `request()->isSecure()` must be true on a secured site.
   - Pinned by `TestFrankenCaddyfileForwardsHostAndScheme` (Task 2).
2. **Port and process cleanup.**
   - Unlinking a site, switching it back to PHP-FPM, or `mullion stop` must kill the FrankenPHP process tree.
   - No orphan process may be left holding the port.
   - Pinned by `TestStopKillsServer` (Task 3) and `TestSwitchBackToFpm` (Task 4).
3. **Config change while running.**
   - Toggling Watch or production-like mode on a FrankenPHP site restarts its server with the new Caddyfile.
   - An unchanged config must not restart it.
   - Pinned by `TestEnsureRestartsOnlyOnChange` (Task 3).
4. **Download failures.**
   - A digest mismatch or a partial download leaves no half-installed `<tag>` folder.
   - A later install retries cleanly.
   - Pinned by `TestInstallRejectsBadDigest` (Task 1).
5. **Worker mode without Octane.**
   - Choosing worker mode on a project without `laravel/octane` in `composer.lock` is refused with a clear message, and never silently ignored.
   - Pinned by `TestWorkerModeNeedsOctane` (Task 4).

---

### Task 1: `internal/frankenphp` — releases and install

**Files:**
- Create: `internal/frankenphp/release.go`
- Create: `internal/frankenphp/install.go`
- Test: `internal/frankenphp/install_test.go`
- Modify: `internal/pmdir/pmdir.go` (add `FrankenDir()`)

**Interfaces:**
- **Produces:**
  - `pmdir.Paths.FrankenDir() string` → `<home>/frankenphp`
  - `var ReleasesAPI = "https://api.github.com/repos/php/frankenphp/releases"`
  - `type Release struct{ Tag string; Asset Asset }`
  - `type Asset struct{ Name, URL, Digest string; Size int64 }`
  - `func Latest(ctx context.Context) (Release, error)`: the newest non-prerelease that has an asset for this GOOS/GOARCH.
  - `func Install(ctx context.Context, paths pmdir.Paths, rel Release, progress func(string)) error`
  - `func Installed(paths pmdir.Paths) []string`: installed tags, newest first.
  - `func Binary(paths pmdir.Paths, tag string) string`: the path to `frankenphp` or `frankenphp.exe` inside the tag folder.
  - `func PHPVersion(bin string) (string, error)`: parses `X.Y.Z` from the `PHP X.Y.Z` part of `<bin> -v`.
  - `func assetName(goos, goarch string) (string, error)`

- [ ] **Step 1: Write failing tests.**
  - `TestAssetName`:
    - darwin/arm64 → `frankenphp-mac-arm64`
    - windows/amd64 → `frankenphp-windows-x86_64.zip`
    - darwin/amd64 → an error containing `Intel Macs`
    - linux → an error
  - `TestLatestSkipsReleasesWithoutAsset`: an httptest server returns two releases. The newest lacks this platform's asset, so `Latest` returns the older one.
  - `TestInstallVerifiesAndPlacesBinary`: the server serves a small fake binary with the right digest.
    - After `Install`, `Binary(paths, tag)` exists and is executable on unix.
    - On Windows the zip is extracted with `internal/archive`.
  - `TestInstallRejectsBadDigest`: with a wrong digest, `Install` errors and neither `FrankenDir()/<tag>` nor its temp folder remains. A second install with the right digest succeeds.
  - `TestPHPVersionParses`: a fake script prints `FrankenPHP v1.14.0 PHP 8.6.0 Caddy v2.10.2`, and `PHPVersion` returns `8.6.0`.
- [ ] **Step 2:** Run `go test ./internal/frankenphp/` → FAIL (package missing).
- [ ] **Step 3: Implement.**
  - Download to `<FrankenDir>/.<tag>.tmp`, verify sha256 while streaming, then rename into place.
  - Remove the temp folder on any error.
  - Report progress as `Downloading FrankenPHP <tag> (<HumanBytes>)…`.
- [ ] **Step 4:** Run `go test ./internal/frankenphp/` → PASS, then `GOOS=windows go vet ./internal/frankenphp/`.
- [ ] **Step 5: Real-binary check (manual).** Download v1.14.0 for mac into the scratchpad and record the real `-v` output.
  - Adjust the `TestPHPVersionParses` fixture to match it exactly.
  - Confirm `php-server --help`, the `php-cli` subcommand, the `frankenphp { php_ini … }` global option, and the `php_server { env … }` subdirective exist (`frankenphp list-modules`, docs).
  - Note any differences in the task report.
- [ ] **Step 6:** Commit: `FrankenPHP: download and install official releases`.

### Task 2: per-site FrankenPHP Caddyfile and main-Caddy proxy block

**Files:**
- Create: `internal/frankenphp/caddyfile.go`
- Test: `internal/frankenphp/caddyfile_test.go`
- Modify: `internal/caddy/caddyfile.go` (new `Kind` `"franken"`, new field `FrankenPort int`)
- Test: `internal/caddy/caddyfile_test.go` (add cases only)

**Interfaces:**
- **Produces:**
  - `type SiteConf struct{ Name, Root string; Port int; Secure bool; Prepend, WatchDir string; Env map[string]string; FreezeOpcache bool }`
  - `func Caddyfile(c SiteConf) string`
  - In `caddy.SiteConf`: `Kind == "franken"` uses `FrankenPort`. `FrankenPort == 0` means the server is down.

- [ ] **Step 1: Write failing tests.**
  - `TestFrankenCaddyfileClassic`: for `Port: 44001, Root: "/p/public"` the output contains:
    - `admin off`, `auto_https off`
    - `http://127.0.0.1:44001`
    - `root * "/p/public"`
    - `php_server`
  - `TestFrankenCaddyfileForwardsHostAndScheme`:
    - With `Secure: true` the `php_server` block has `env HTTPS on` and `env SERVER_PORT 443`.
    - With `Secure: false` neither appears.
    - The global options have `servers { trusted_proxies static 127.0.0.1/32 ::1/128 }`.
  - `TestFrankenCaddyfileWatchAndProd`:
    - With `Prepend` set → the global `frankenphp { php_ini auto_prepend_file "<prepend>" }`.
    - With `WatchDir` set → `env MULLION_WATCH "<dir>"` and `env MULLION_RID {http.request.header.X-Mullion-Rid}`.
    - `Env{"APP_ENV":"production"}` → `env APP_ENV "production"`, with keys sorted.
    - `FreezeOpcache` → `php_ini opcache.validate_timestamps 0`.
  - In `internal/caddy`, `TestGenerateFrankenSite`:
    - With `Kind:"franken", FrankenPort:44001` the block holds `reverse_proxy 127.0.0.1:44001 {` with `header_up X-Mullion-Rid {http.request.uuid}`, and `flush_interval -1` (streamed responses).
    - It must **not** rewrite Host.
    - With `FrankenPort:0` it holds `respond "FrankenPHP for this site is not running — open the Mullion panel or run ` + "`mullion start`" + `." 503`.
- [ ] **Step 2:** Run `go test ./internal/frankenphp/ ./internal/caddy/` → FAIL.
- [ ] **Step 3: Implement both renderers.**
  - Reuse `quote` semantics: write a local `quote` in frankenphp equivalent to caddy's.
  - The main-Caddy franken block also writes the same `log { output file … }` line as other kinds, so Watch's request tail keeps working.
- [ ] **Step 4:** Run `go test ./internal/frankenphp/ ./internal/caddy/` → PASS. All existing caddy tests stay green and unmodified.
- [ ] **Step 5:** Commit: `FrankenPHP: per-site Caddyfile and proxy block`.

### Task 3: FrankenPHP server process (start, restart on change, stop)

**Files:**
- Create: `internal/frankenphp/server.go`
- Create: `internal/frankenphp/procs_other.go`
- Create: `internal/frankenphp/procs_windows.go`
- Test: `internal/frankenphp/server_test.go` (`//go:build !windows`, uses a fake `frankenphp` shell script that runs `python3 -m http.server` on the port from its config, the same approach as `internal/devserver/fallback_test.go`)

**Interfaces:**
- **Consumes:** `Binary`, `Caddyfile`, `SiteConf` from Tasks 1 and 2.
- **Produces:**
  - `type Run struct{ Paths pmdir.Paths; Bin string; Site SiteConf; ProjectDir string; Worker bool; PHPCLI []string }`
    - `PHPCLI` is the argv prefix for artisan in worker mode: `[<bin>, "php-cli"]`.
  - `func Ensure(r Run) (int, error)`: returns the listening port. It writes `<PidsDir>/franken-<site>.pid`, `.hash` and `<home>/frankenphp/sites/<site>/Caddyfile`. Output goes to the log `<LogsDir>/franken-<site>.log`.
  - `func Stop(paths pmdir.Paths, site string)`: kills the process tree and removes the pid and hash files.
  - `func StopAll(paths pmdir.Paths)`
  - `func Running(paths pmdir.Paths, site string) bool`
  - `func AssignPort(used map[int]bool) int`: the first free port from 44001.
- **Behaviour:**
  - The hash is sha256 of (the Caddyfile text + `Worker` + `Bin`). If the process is alive and the hash is unchanged, nothing happens. Otherwise Stop, then start.
  - **Classic mode:** `<bin> run --config <Caddyfile> --adapter caddyfile`.
  - **Worker mode:** `<bin> php-cli artisan octane:frankenphp --host=127.0.0.1 --port=<port> --admin-port=<port+1000> --no-interaction`.
    - It runs in `ProjectDir`, with `PATH` prefixed by `filepath.Dir(Bin)` so Octane finds the binary.
    - `Site.Env` is added to the process env.
  - **Readiness:** a TCP dial on `127.0.0.1:<port>`, 30 s deadline.
    - If the process dies or the deadline passes, the error carries the last 8 log lines, and a `---- <time>: mullion: <reason> ----` line is appended to the log (the same convention as `devserver.Ensure`).
  - **Process helpers:** `processAlive` and `killTree` copy `internal/devserver/procs_*.go` (`tasklist` and `taskkill /T` on Windows; process group on unix).

- [ ] **Step 1: Write failing tests.**
  - `TestEnsureStartsAndReportsPort`
  - `TestEnsureRestartsOnlyOnChange`:
    - Same `Run` twice → same pid.
    - Change `Site.Env` → new pid, and the old pid is gone.
  - `TestStopKillsServer`: after `Stop`, the port no longer accepts connections and the pid file is gone.
  - `TestEnsureFailureLandsInLog`: a fake binary that exits 1 makes `Ensure` error, and the log contains `mullion:`.
- [ ] **Step 2:** Run `go test ./internal/frankenphp/ -run 'Ensure|Stop'` → FAIL.
- [ ] **Step 3:** Implement per the interfaces above.
- [ ] **Step 4:** Run → PASS, then `GOOS=windows go vet ./internal/frankenphp/`.
- [ ] **Step 5:** Commit: `FrankenPHP: supervised per-site server`.

### Task 4: wire into the app (config, Apply, unlink, stop, CLI)

**Files:**
- Modify: `internal/config/config.go`
  - Add `Server string json:"server,omitempty"` to `Site`: `""` means php-fpm, `"frankenphp"`.
  - Add `Franken *Franken json:"franken,omitempty"` with `Franken{ Tag string; Worker bool; Port int }`.
  - Add `func (s Site) OnFranken() bool`.
- Create: `internal/app/franken.go`
- Modify: `internal/app/app.go` (`WriteCaddyfile` php branch, `Apply`)
- Modify: the unlink path (`grep -n "func (a \*App) Unlink\|devserver.Stop" internal/app`) and the `mullion stop` path (`grep -rn "devserver.StopAll" cmd internal`)
- Create: `cmd/franken.go`
- Test: `internal/app/franken_test.go`

**Interfaces:**
- **Consumes:** everything from Tasks 1–3.
- **Produces:**
  - `func (a *App) SetSiteServer(ctx context.Context, name, server string, worker bool, progress func(string)) error`
    - Validates the input, installs the latest FrankenPHP when none is installed, assigns a port, then runs `Apply`.
  - `func (a *App) EnsureFrankenServers() map[string]int`: called in `Apply` right after `EnsureDevServers`, and passed to `WriteCaddyfile` (extend its parameter to a struct or a second map).
  - `func (a *App) FrankenInfo(name string) (FrankenInfo, error)` with `FrankenInfo{ Server string; Worker, OctaneInstalled, Running bool; Tag, PHPVersion, Log string; Installed []string; Port int }`.
  - CLI:
    - `mullion server <site> [fpm|frankenphp] [--worker]`
    - `mullion server <site>` prints the current state.
    - `mullion frankenphp update` installs the latest release and restarts the FrankenPHP sites.
- **Rules:**
  - Only PHP sites (`IsPHP()`) can be switched. Any other kind is refused with `only PHP sites can run on FrankenPHP`.
  - Worker mode requires `laravel/octane` in the project's `composer.lock`. Otherwise the error is exactly `worker mode needs Laravel Octane — run: composer require laravel/octane`.
  - **Switching to FrankenPHP:**
    - `Franken.Tag` is the newest installed tag at switch time. Pinned *(call)*: updates only change it through `mullion frankenphp update`.
    - `Franken.Port` comes from `frankenphp.AssignPort`, avoiding every site's `DevPort` and `Franken.Port`.
  - **Switching back to fpm:** calls `frankenphp.Stop` and clears `Server` and `Franken`.
  - **SiteConf mapping:** a FrankenPHP site's `frankenphp.SiteConf` gets:
    - `Root = caddy.DocRoot(s.Path)`
    - `Prepend` and `WatchDir` from `watchCaddyConf(s)`
    - `Env` from `prodCaddyConf(s).Env`
    - `FreezeOpcache = s.ProdMode()`
  - **Profiler:** SPX does not apply to FrankenPHP sites. `ProfilerFastCGIEnv` is not used for them, and the UI says so in Task 5.
  - **Lifecycle:** unlink and rename stop the old server. `mullion stop` calls `frankenphp.StopAll`. `NeededVersions()` is unchanged; the site's pinned PHP stays installed for its CLI.

- [ ] **Step 1: Write failing tests**, using `redisTestApp`-style helpers with temp paths and `skipApply: true`, and a fake installed tag folder:
  - `TestSetSiteServerFranken`: the config has `Server=="frankenphp"`, a port ≥ 44001, and the tag set. `WriteCaddyfile` output for the site contains `reverse_proxy 127.0.0.1:<port>` and no `php_fastcgi`.
  - `TestSwitchBackToFpm`: `Server` is cleared, the Caddyfile has `php_fastcgi` again, and the Stop hook was called (inject `var stopFranken = frankenphp.Stop`).
  - `TestWorkerModeNeedsOctane`: without `composer.lock` → the exact error above. With a lock listing `laravel/octane` → OK.
  - `TestOnlyPHPSites`: a node site → refused.
  - `TestFrankenPortsAvoidCollisions`: an existing `DevPort` and Franken port → the new port is different.
- [ ] **Step 2:** Run `go test ./internal/app/ -run Franken` → FAIL.
- [ ] **Step 3:** Implement the code and the CLI. `mullion server` follows the style of `cmd/isolate.go`.
- [ ] **Step 4:** Run `go vet ./... && go test ./...` → PASS, then `GOOS=windows go vet ./...`.
- [ ] **Step 5:** Commit: `Sites can run on FrankenPHP: per-site server choice, worker mode for Octane`.

### Task 5: project page UI

**Files:**
- Modify: `internal/ui/ui.go`
  - `POST /api/sites/server {name, server, worker}` runs as a job through `startApplyJob`, because the first switch downloads FrankenPHP.
  - `GET /api/sites/franken?name=` returns `FrankenInfo`.
  - `POST /api/frankenphp/update` runs as a job.
- Modify: `internal/ui/project.js` (`renderSettings()`, around line 4722)
- Test: `internal/ui/ui_test.go` (add one API test following the existing handler tests)

**Interfaces:**
- **Consumes:** `SetSiteServer`, `FrankenInfo`, and the update function from Task 4.

**UI:**
- **Settings tab** (PHP sites only): a new row "Server — for this site", placed directly under "PHP version", with a select of `PHP-FPM (default)` / `FrankenPHP`.
- **When FrankenPHP is selected,** show a sub-line with:
  - `FrankenPHP <tag> · PHP <bundled version>` and the running state
  - a "Worker mode (Octane)" checkbox, disabled with a tooltip when Octane isn't installed
  - a "View log" link that opens the log tail in the existing log modal
  - an "Update FrankenPHP" button when a newer release exists
- **Hint text:** "FrankenPHP has its own PHP (<ver>). Artisan, workers and Composer still use PHP <site PHP>. The profiler isn't available on FrankenPHP."
- **Performance tab:** the profiler section shows that same sentence instead of its controls when the site is on FrankenPHP.

- [ ] **Step 1:** Write a failing API test: `/api/sites/server` with server `"bogus"` → 400 or an error containing `server must be fpm or frankenphp`.
- [ ] **Step 2:** Implement the handlers and UI.
- [ ] **Step 3:** Run `go test ./internal/ui/` → PASS.
- [ ] **Step 4:** Commit: `Project page: choose PHP-FPM or FrankenPHP per site`.

### Task 6: real-world verification and docs

- [ ] **Step 1:** In a sandbox HOME (see memory: Mac test environment), with the real v1.14.0 mac binary from Task 1 Step 5:
  - Create a minimal PHP project (an `index.php` that echoes `$_SERVER['HTTP_HOST']`, `$_SERVER['HTTPS'] ?? 'off'`, and `PHP_VERSION`).
  - Run `frankenphp.Ensure` against it, then curl `127.0.0.1:<port>` with `Host: demo.test` and `X-Forwarded-Proto: https`.
  - Expect `demo.test on 8.6.x` when `Secure`.
  - Record the output.
- [ ] **Step 2:** Add a `CHANGELOG.md` entry under `## Unreleased` / Added: "Run a PHP site on FrankenPHP (project Settings → Server, or `mullion server <site> frankenphp`), with worker mode for Laravel Octane."
- [ ] **Step 3:** `go vet ./... && go test ./... && GOOS=windows go vet ./... && GOOS=windows go test -c ./internal/frankenphp/ -o /dev/null`
- [ ] **Step 4:** Commit: `FrankenPHP: changelog`.
