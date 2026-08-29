# Mullion — the zero-setup local dev environment

One binary that runs your whole local development world on **Windows**
and **macOS**: PHP backends, JavaScript frontends, Node versions, MySQL,
`.test` domains, and trusted local HTTPS — with no Docker, no Homebrew
dependencies, and no config files to learn.

```
cd my-laravel-api  && mullion link   →  https://my-laravel-api.test
cd my-vite-app     && mullion link   →  https://my-vite-app.test
```

That's the entire workflow. Mullion picks the right PHP or Node version
per project, runs `npm run dev` for you behind the scenes, wakes
sleeping projects the moment you open their link, and puts idle ones
back to sleep so your machine stays light.

## What you get

- **PHP version manager** — install any version, switch system-wide
  (`mullion use 8.4`) or pin one project (`mullion isolate 7.4`); every
  version runs side by side
- **Node version manager** — same model (`mullion node use 24`,
  `mullion node isolate 18`); the `node`/`npm` on your PATH resolve
  per project automatically (pinned version → `.nvmrc` → default)
- **Frontend dev servers, managed** — `mullion link` in a Vite/React/
  Vue/Next project and you never type `npm run dev` again: opening the
  link starts it (with a live "starting…" page), closing your tabs puts
  it back to sleep after 2 minutes, an idle open tab after 10 —
  requests and code edits keep it awake
- **dev / build switch per domain** — flip a frontend domain between
  the live dev server and the last production build (`mullion serve
  build`, or a dropdown in the panel); Mullion builds the project
  itself when there is no build yet
- **`.test` domains + trusted HTTPS** for every project, via
  [Caddy](https://caddyserver.com) and its local CA
- **MySQL + phpMyAdmin** — installed, initialized, and running; manage
  the root password and databases from the CLI or the panel; replacing
  an existing server (Laragon, XAMPP, a brew service) always offers a
  backup first and imports it into Mullion
- **A real control panel** (`mullion ui`) — light/dark, with dedicated
  pages for backend projects, frontend projects, PHP, Node, and the
  database
- **Self-healing** — `mullion doctor` diagnoses the whole stack;
  services recover on their own, and competitors squatting your ports
  (old stacks, resurrected brew services, stale processes) are detected
  and replaced with consent

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
(`C:\Mullion` on Windows, `~/.mullion` on macOS), downloads Caddy, puts
mullion on your PATH, installs the **latest PHP** (system default), the
**latest Composer**, the **latest Node LTS** (with npm), the **latest
MySQL** (initialized and running on `127.0.0.1:3306`, user `root`, no
password), and serves **phpMyAdmin** at `https://phpmyadmin.test` with a
trusted certificate. **Open a new terminal afterwards**, and from then
on plain `mullion` (and `php`, `composer`, `node`, `npm`) work from any
directory.

On Windows, setup asks for administrator rights **once** (a single UAC
prompt). On macOS the equivalent is a single password prompt. Setup also
asks whether Mullion should start automatically when you sign in, and
offers to take over from existing stacks it finds — importing their
databases before touching anything. It is idempotent: every step already
done is skipped, so re-running it is always safe.

## Quick start

**A PHP project** (Laravel `public/` auto-detected):

```
cd ~/code/my-api               # C:\code\my-api on Windows
mullion link                   # http://my-api.test
mullion secure                 # https://my-api.test (trusted local cert)
```

**A frontend project** — Mullion owns the dev server for you:

```
cd ~/code/my-vite-app
mullion link                   # https://my-vite-app.test — deps installed, dev server managed
mullion serve build            # the same domain serves the production build (builds it if needed)
mullion serve dev              # back to the live dev server
mullion link --build           # OR a separate <name>-build.test to compare build vs dev side by side
```

**Another runtime version for one legacy project:**

```
mullion php install 7.4  && mullion isolate 7.4        # this project runs PHP 7.4
mullion node install 18  && mullion node isolate 18    # this project runs Node 18
```

## How frontend sites live and sleep

Opening a frontend link **always works** — that's the contract:

- The domain's dev server is down? Caddy hands the request to Mullion's
  tiny background agent, you see a live *"Starting…"* page for a few
  seconds, and land in the app. If the server can't start, the page
  shows the actual error and log tail instead of spinning.
- No tab open on the site? It goes back to **sleep after 2 minutes**,
  freeing RAM and CPU.
- A tab is open but idle? **10 minutes** of no requests *and no code
  edits* — an active coding session is never killed under you.
- `mullion dev stop` (or the panel's Stop button, with confirmation)
  puts a site to sleep immediately; opening the link wakes it again.

`mullion status` shows each site's state: `running` (with its port and
Node version), `sleeping`, or serving the production build.

## Commands

| Command | What it does |
|---|---|
| `mullion setup` | Full first-time setup: PATH, Caddy, latest PHP + Composer + Node LTS + MySQL + phpMyAdmin |
| `mullion link [name]` | Serve the current directory at `https://<name>.test` (project type auto-detected) |
| `mullion link --build [--dir d]` | Serve the last production build as `<name>-build.test` (auto-detects dist/build/out) |
| `mullion unlink` / `links` / `rename [old] <new>` | Remove, list, or rename sites (rename also lives in the panel) |
| `mullion secure` / `unsecure [name]` | HTTPS with a locally-trusted cert / back to HTTP |
| `mullion serve dev\|build [name]` | Switch what a frontend domain serves |
| `mullion dev start` / `stop` / `restart` `[name]` | Control one dev server (`stop` asks, then it sleeps until the link wakes it) |
| `mullion php install <v>` / `list` / `available` / `uninstall` | Manage PHP versions (windows.php.net / static-php.dev builds) |
| `mullion use <v>` / `isolate <v>` / `unisolate` | System-wide PHP / pin one project / unpin |
| `mullion php ext list\|enable\|disable\|get` | Extensions (toggling + PECL on Windows; compiled-in list on macOS) |
| `mullion node install [v]` / `use` / `list` / `available` / `uninstall` | Manage Node versions (official nodejs.org builds) |
| `mullion node isolate <v>` / `unisolate` / `which` | Pin a project's Node / unpin / explain what resolves here and why |
| `mullion node npm <v> [node]` | Change the npm version inside a Node install |
| `mullion mysql install [v]` | Install/switch MySQL (8.4 LTS default; databases migrate automatically; MariaDB on Windows) |
| `mullion mysql password [pw]` / `start` / `stop` / `restore <f>` | Root password (phpMyAdmin follows) / control / import dumps |
| `mullion db list` / `create <n>` / `drop <n>` | Manage databases (also in the panel) |
| `mullion composer install [v]` / `phpmyadmin [v]` | Composer / phpMyAdmin at `https://phpmyadmin.test` |
| `mullion heidisql` | HeidiSQL desktop client (Windows only) |
| `mullion start` / `stop` / `restart` / `status` | Control the whole stack |
| `mullion doctor` | Full diagnosis — paste its output when reporting a problem |
| `mullion ui` | The control panel (runs in the background; your terminal stays free) |
| `mullion tld [tld]` / `autostart [on\|off]` | Domain suffix / start at sign-in |
| `mullion uninstall` | Remove everything (offers a database backup first; project folders untouched) |

## The control panel

`mullion ui` opens the panel in your default browser — no extra windows,
no terminal held hostage. Light and dark themes, with dedicated pages:

- **Overview** — stack health and one-click start/stop
- **Backend / Frontend** — each project family on its own page: link,
  rename, HTTPS, per-site PHP or Node version, dev/build dropdown,
  dev-server Start/Stop
- **PHP / Node** — install versions, switch defaults, manage extensions
- **Database** — server control, root password, and a proper databases
  table with create/drop

## How it works

- Everything lives in one folder — `C:\Mullion\` on Windows,
  `~/.mullion/` on macOS: PHP in `php/<version>`, Node in
  `node/<version>`, Caddy and mullion in `bin/`, logs in `logs/`.
- **Version switching**: `php/current` and `node/current` are junctions
  (Windows) / symlinks (macOS) on your PATH — switching is instant, no
  admin rights. `node`/`npm`/`npx` are smart shims that resolve the
  right version for the directory you're in.
- **PHP serving**: every PHP version runs its own FastCGI worker
  (`php-cgi` on Windows, `php-fpm` on macOS) on a version-derived port
  (8.3 → 9083). Caddy routes each site to *its* version's port.
- **Frontend serving**: Mullion runs the project's dev server (npm,
  pnpm, or yarn — detected from the lockfile; dependencies installed on
  first run), detects the port it actually opened, and reverse-proxies
  the domain to it — HMR/websockets included. A tiny background agent
  provides wake-on-demand and idle sleep. Build mode serves the output
  directory statically with SPA fallback.
- **Domains**: linked sites get entries in the hosts file, kept inside
  a clearly-marked managed block (one UAC/password prompt).
- **HTTPS**: Caddy's internal CA issues certificates and installs its
  root into the system trust store once — browsers show a real padlock.
- **PHP builds**: Windows uses official windows.php.net zips with
  per-extension toggling and PECL downloads. macOS uses
  [static-php.dev](https://static-php.dev)'s dependency-free static
  builds with the common extension set compiled in — opcache, intl,
  imagick, redis, sodium, and the pdo_mysql / pdo_sqlite / pdo_pgsql
  drivers included.

## Troubleshooting

**Start with `mullion doctor`** — it checks the binary, ports and who
owns them, Caddy's identity, PHP/Node/MySQL, the wake agent, and every
site, marking problems in red with the fix.

**`php -v` or `node -v` shows another version.** Another install sits
earlier on your PATH (Laragon/XAMPP in the system PATH on Windows; nvm
or Homebrew on macOS). `mullion use` / `mullion node use` detect the
shadow and offer to disable the offending PATH entry for you — then
open a NEW terminal. `mullion node which` explains exactly which Node
resolves in the current directory and why.

**A frontend link shows the error page instead of the app.** The real
reason (with the dev server's log tail) is printed on that page; fix it
and refresh. `mullion doctor` shows the same details.

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
