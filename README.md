# Mullion — PHP version manager & local dev server for Windows

A local PHP development environment for Windows 10/11 (64-bit), shipped
as a single `mullion.exe` with no dependencies — setup even installs the
Microsoft VC++ runtime (needed by PHP and MySQL) when a clean machine
lacks it. It installs PHP versions straight from windows.php.net,
switches them system-wide or per project, installs Composer and
MySQL/MariaDB, serves your projects on `.test` domains through
[Caddy](https://caddyserver.com), and secures them with trusted local
HTTPS certificates — all driven from a desktop control panel or the
terminal.

## Install

**[⬇ Download `mullion.exe` from the latest release](https://github.com/anabiiil/mullion/releases/latest)** —
one file, nothing else to install. (This repository is the source code;
the ready-to-run exe lives on the Releases page.)

Then **double-click it** and confirm `Run setup now?` — or run it from a
terminal:

```
mullion.exe setup
```

> Windows SmartScreen may warn about the new unsigned exe on first run —
> click **More info → Run anyway**.

One command sets up the whole stack: it creates `C:\Mullion`,
downloads Caddy, copies `mullion.exe` into `C:\Mullion\bin`, adds the right folders
to your user PATH, installs the **latest PHP** (and makes it the system
default), the **latest Composer**, the **latest MySQL** (initialized and
running on `127.0.0.1:3306`, user `root`, no password), and serves
**phpMyAdmin** at `https://phpmyadmin.test` with a trusted certificate.
**Open a new terminal afterwards**, and from then on plain `mullion` (and
`php`, `composer`) work from any directory.

Setup asks for administrator rights **once** (a single UAC prompt) and
continues in a new elevated window — no more prompt per step. It also
asks whether Mullion should start automatically when you sign in to Windows
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
cd C:\code\myapp
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
| `mullion php available` | List versions installable from windows.php.net |
| `mullion php install <v>` | Install a version (`8.3`, `8.3.30`, even EOL ones like `7.4`) |
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
| `mullion mysql install [v]` | Install MySQL (newest 8.4 LTS by default; `latest`, a branch, or an exact version) — switching versions migrates your databases automatically |
| `mullion mysql start` / `stop` / `uninstall` | Control / remove the MySQL server |
| `mullion composer install [v]` | Install Composer (latest by default, or a specific version) |
| `mullion phpmyadmin [v]` | Install phpMyAdmin (latest by default) at `https://phpmyadmin.test` |
| `mullion heidisql` | Install (first use) and open the HeidiSQL desktop client |
| `mullion start` / `stop` / `restart` / `status` | Control the background services |
| `mullion autostart [on\|off]` | Start Mullion automatically at Windows sign-in |
| `mullion ui` | Open the control panel window (double-clicking `mullion.exe` does the same once set up) |
| `mullion uninstall` | Remove everything (offers a databases backup first; project folders untouched) |

## How it works

- Everything lives in `C:\Mullion\` — PHP versions in `php\<version>`,
  Caddy and mullion in `bin\`, logs in `logs\`.
- **Global switching**: `php\current` is a directory junction pointing at the
  active version; that junction is on your PATH, so switching is instant and
  needs no admin rights.
- **Per-project versions**: every PHP version runs its own `php-cgi` FastCGI
  process on a version-derived port (8.3 → 9083, 7.4 → 9074). Caddy routes
  each site to the port of *its* version, so different projects run different
  PHP versions side by side.
- **Domains**: linked sites get entries in the Windows hosts file, kept inside
  a clearly-marked managed block. Writing it triggers one UAC prompt.
- **HTTPS**: Caddy's internal CA issues certificates for secured sites and
  installs its root into the Windows trust store (one-time UAC prompt), so
  browsers show a proper padlock.

## Troubleshooting

**`php -v` still shows another version after `mullion use`.** Another PHP
install (Laragon, XAMPP, ...) sits earlier on your PATH. Windows always
searches the *system* PATH before the user PATH where Mullion lives, so a
system-wide entry like `C:\laragon\bin\php\...` wins no matter what Mullion
does. `mullion use` and `mullion status` detect this and print the exact offending
path; remove that directory from the system Path (Settings > System >
About > Advanced system settings > Environment Variables), then open a
new terminal.

**A linked site doesn't open.** `mullion link` and `mullion secure` start the
servers themselves, so this should not happen anymore; check `mullion status`
and `C:\Mullion\logs\caddy.log`. Sites also keep working after you close the
terminal — Caddy runs fully detached.

## Building from source

```
GOOS=windows GOARCH=amd64 go build -trimpath -o dist/mullion.exe .
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
