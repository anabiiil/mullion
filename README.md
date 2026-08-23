# Mullion — PHP version manager & local dev server

A local PHP development environment for **Windows 10/11** and **macOS**,
shipped as a single binary with no dependencies — no Homebrew, no
installers. It installs PHP versions (from windows.php.net on Windows,
from [static-php.dev](https://static-php.dev)'s dependency-free static
builds on macOS), switches them system-wide or per project, installs
Composer and MySQL, serves your projects on `.test` domains through
[Caddy](https://caddyserver.com), and secures them with trusted local
HTTPS certificates — all driven from a desktop control panel or the
terminal.

## Install

**[⬇ Download the latest release](https://github.com/anabiiil/mullion/releases/latest)** —
one file, nothing else to install. (This repository is the source code;
the ready-to-run binaries live on the Releases page.)

### Windows

With [Scoop](https://scoop.sh):

```
scoop bucket add mullion https://github.com/anabiiil/scoop-bucket
scoop install mullion
mullion setup
```

Or download `mullion.exe`, then **double-click it** and confirm
`Run setup now?` — or run it from a terminal:

```
mullion.exe setup
```

> Windows SmartScreen may warn about the new unsigned exe on first run —
> click **More info → Run anyway**.

### macOS

With [Homebrew](https://brew.sh):

```bash
brew tap anabiiil/tap
brew trust anabiiil/tap   # newer brew versions require this once for third-party taps
brew install mullion
mullion setup
```

Or without Homebrew, one command downloads the right binary and runs setup:

```bash
curl -fsSL https://raw.githubusercontent.com/anabiiil/mullion/main/install.sh | sh
```

Or download the tarball for your Mac from the Releases page
(`darwin-arm64` for Apple Silicon, `darwin-amd64` for Intel), then:

```bash
tar -xzf mullion-*-darwin-*.tar.gz
xattr -c ./mullion   # clear the quarantine flag Gatekeeper puts on downloads
./mullion setup
```

(brew-installed binaries need no `xattr` step.) Setup may ask for your
password once — for `/etc/hosts` entries and for trusting the local
HTTPS certificate.

One command sets up the whole stack: it creates the install directory
(`C:\Mullion` on Windows, `~/.mullion` on macOS), downloads Caddy, copies
the mullion binary into its `bin` folder, adds the right folders to your
PATH, installs the **latest PHP** (and makes it the system default), the
**latest Composer**, the **latest MySQL** (initialized and running on
`127.0.0.1:3306`, user `root`, no password), and serves **phpMyAdmin**
at `https://phpmyadmin.test` with a trusted certificate.
**Open a new terminal afterwards**, and from then on plain `mullion` (and
`php`, `composer`) work from any directory.

On Windows, setup asks for administrator rights **once** (a single UAC
prompt) and continues in a new elevated window — no more prompt per
step. On macOS the equivalent is a single sudo password prompt. Setup
also asks whether Mullion should start automatically when you sign in
(change it any time with `mullion autostart on|off`).

Setup is idempotent — every step it finds already done is skipped, so
re-running it is safe.

> Double-clicking `mullion.exe` in Explorer also works: before setup it offers
> to run setup for you; after setup it opens the **control panel** — a
> desktop-style window for managing services, PHP versions, MySQL, and
> sites (same as `mullion ui`).

## Quick start

After `mullion setup`, PHP, Composer, MySQL, and phpMyAdmin are already there —
serving a project is all that's left:

```
cd ~/code/myapp                # C:\code\myapp on Windows
mullion link                   # serve at http://myapp.test (Laravel public/ auto-detected)
mullion secure                 # upgrade to https://myapp.test (trusted local cert)
```

Need another PHP version (e.g. a legacy project)?

```
mullion php install 7.4        # any version, even EOL ones
mullion use 7.4                # switch the system default, or:
mullion isolate 7.4            # pin just this project to it
```

## Commands

| Command | What it does |
|---|---|
| `mullion setup` | Full first-time setup: PATH, Caddy, latest PHP + Composer + MySQL + phpMyAdmin |
| `mullion php available` | List installable versions (windows.php.net / static-php.dev) |
| `mullion php install <v>` | Install a version (`8.3`, `8.3.30`; on Windows even EOL ones like `7.4` — macOS builds start at 8.0) |
| `mullion php list` | List installed versions (`*` = active) |
| `mullion php uninstall <v>` | Remove an installed version |
| `mullion use <v>` | Switch the **system-wide** PHP version |
| `mullion isolate <v>` | Pin the **current project** to a version (e.g. legacy app on 7.4) |
| `mullion unisolate` | Project follows the global version again |
| `mullion link [name]` | Serve current directory at `http://<name>.test` |
| `mullion unlink [name]` | Stop serving it |
| `mullion links` | List all linked sites |
| `mullion secure [name]` | Serve a site over HTTPS (locally-trusted cert) |
| `mullion unsecure [name]` | Back to plain HTTP |
| `mullion tld [tld]` | Show or change the domain suffix (default `.test`) |
| `mullion mysql install [v]` | Install MySQL (newest 8.4 LTS by default; `latest`, a branch, or an exact version) — switching versions migrates your databases automatically. MariaDB: Windows only |
| `mullion mysql start` / `stop` / `uninstall` | Control / remove the MySQL server |
| `mullion composer install [v]` | Install Composer (latest by default, or a specific version) |
| `mullion phpmyadmin [v]` | Install phpMyAdmin (latest by default) at `https://phpmyadmin.test` |
| `mullion heidisql` | Install (first use) and open the HeidiSQL desktop client (Windows only) |
| `mullion start` / `stop` / `restart` / `status` | Control the background services |
| `mullion autostart [on\|off]` | Start Mullion automatically at sign-in (Run key on Windows, launchd agent on macOS) |
| `mullion ui` | Open the control panel window (double-clicking `mullion.exe` does the same once set up) |
| `mullion uninstall` | Remove everything (offers a databases backup first; project folders untouched) |

## How it works

- Everything lives in one folder — `C:\Mullion\` on Windows, `~/.mullion/`
  on macOS: PHP versions in `php/<version>`, Caddy and mullion in `bin/`,
  logs in `logs/`.
- **Global switching**: `php/current` is a directory junction (Windows) or
  symlink (macOS) pointing at the active version; it is on your PATH, so
  switching is instant and needs no admin rights.
- **Per-project versions**: every PHP version runs its own FastCGI worker
  (`php-cgi` on Windows, `php-fpm` on macOS) on a version-derived port
  (8.3 → 9083, 7.4 → 9074). Caddy routes each site to the port of *its*
  version, so different projects run different PHP versions side by side.
- **Domains**: linked sites get entries in the hosts file
  (`C:\Windows\...\etc\hosts` / `/etc/hosts`), kept inside a
  clearly-marked managed block. Writing it triggers one UAC prompt on
  Windows, one sudo/password prompt on macOS.
- **HTTPS**: Caddy's internal CA issues certificates for secured sites and
  installs its root into the system trust store (one-time UAC/password
  prompt), so browsers show a proper padlock.
- **PHP builds**: Windows uses the official windows.php.net zips, with
  per-extension enable/disable and PECL downloads (`mullion php ext ...`).
  macOS uses static-php.dev's static builds, which have the common
  extension set compiled in — including opcache, intl, imagick, redis,
  sodium, and the pdo_mysql / pdo_sqlite / pdo_pgsql drivers — so there
  is nothing to toggle (`mullion php ext list` shows what's built in).

## Troubleshooting

**`php -v` still shows another version after `mullion use`.** Another PHP
install sits earlier on your PATH. On Windows the culprit is usually a
dev stack (Laragon, XAMPP, ...) in the *system* PATH, which is always
searched before the user PATH where Mullion lives — remove that directory
from the system Path (Settings > System > About > Advanced system
settings > Environment Variables) and open a new terminal. On macOS it
is usually a Homebrew or other php on the PATH — open a NEW terminal so
Mullion's profile entry (prepended to PATH) takes effect. `mullion use`
and `mullion status` detect the shadow and print the exact offending path.

**A linked site doesn't open.** `mullion link` and `mullion secure` start the
servers themselves, so this should not happen anymore; check `mullion status`
and `logs/caddy.log` in the install directory. Sites also keep working
after you close the terminal — Caddy runs fully detached.

## Building from source

```
GOOS=windows GOARCH=amd64 go build -trimpath -o dist/mullion.exe .
GOOS=darwin  GOARCH=arm64 go build -trimpath -o dist/mullion .
```

(The binary is deliberately not stripped with `-ldflags "-s -w"` — stripped
Go executables trip antivirus heuristics far more often. The exe icon and
version metadata come from `rsrc_windows_amd64.syso`; regenerate it after
editing `versioninfo.json` / `mullion.ico` / `mullion.manifest` with
`go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest -64 -o rsrc_windows_amd64.syso versioninfo.json`.)

Run the tests with `go test ./...`.

**Antivirus false positives:** unsigned Go binaries are commonly flagged.
The build already embeds an icon, version metadata, and a manifest (all of
which reduce false positives), but the reliable fix for distribution is
signing `mullion.exe` with an Authenticode code-signing certificate
(`signtool sign /fd SHA256 /tr <timestamp-url> /td SHA256 mullion.exe`)
and submitting false-positive reports to AV vendors.
