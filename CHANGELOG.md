# Changelog

All notable changes to Mullion. Versions follow [semantic versioning](https://semver.org).

## 2.1.1 — 2026-10-02

### Fixed

- **The project page shows only your chosen terminal's button**: "Mullion
  Terminal" when Mullion Terminal is your terminal, "Terminal here" when
  it's the built-in one, instead of both. The project's Terminal tab stays
  either way, and pages already open update as soon as you switch in
  Settings → Terminal.

## 2.1.0 — 2026-10-02

Mullion Terminal as your terminal, project tabs on the Sites page, and a
round of Windows fixes: Mullion now recognizes its own servers after
setup, `mullion restart` no longer breaks MySQL, and the Windows icon
matches the app icon.

### Added

- **Mullion Terminal.** The panel can use the standalone Mullion Terminal
  app as your terminal. A first-run dialog asks which terminal you want
  and installs Mullion Terminal if you pick it. Settings → Terminal
  switches between the two and installs or updates the app. Terminal
  buttons then open the project folder as a new tab there; a project
  page's own Terminal tab stays built-in and gains an "Open in Mullion
  Terminal" button. New CLI commands: `mullion terminal [path|site]` and
  `mullion terminal install`.
- **Project tabs live on the Sites page.** Open projects show as tabs
  next to a fixed "All" tab, and the separate "Projects" sidebar entry is
  gone. Clicking "Sites" in the sidebar closes every project tab.

### Changed

- **Windows icon.** The exe, taskbar, tray and panel window use the app
  icon's look (a midnight tile with the coral mark) instead of the bare
  coral square. Explorer's Properties now shows the right version.
- **Faster Ports page on Windows.** Port owners are read straight from
  Windows instead of one PowerShell call per port (about 7s → 0.3s).

### Fixed

- **Windows: Mullion treated its own servers as foreign.** Setup runs as
  administrator, so the servers it started did too, and Windows hides
  their paths from normal programs. `mullion doctor` reported a "FOREIGN
  caddy" and a foreign MySQL, the panel's Start MySQL refused, and the
  Ports page listed Mullion's own ports as conflicts. Mullion now
  recognizes them, and setup hands the servers back to your normal
  session when it finishes, so restart, the panel and the tray can
  manage them.
- **`mullion restart` could leave MySQL stopped** ("ibdata1 must be
  writable"): it started the new server while the old one was still
  closing its data files. Stopping MySQL now waits for it to exit.
- **Choosing Mullion Terminal could be forgotten**, sending the sidebar's
  Terminal back to the built-in page: two settings saved at once
  overwrote each other.
- **`mullion link` after a declined UAC prompt** said "already links"
  instead of retrying the hosts file update. Linking the same folder
  again now retries, in the CLI and the panel.
- **Composer showed as "not installed"** on the PHP page when the panel
  was started from Git Bash or Windows Terminal (colored version output).
- Windows paths in the generated Caddyfile no longer have doubled
  backslashes.
- Settings showed "—" for free space before the first backup existed,
  and "Show in Finder" on Windows when opened first. The Sites page says
  php-cgi (FastCGI) on Windows instead of php-fpm.

## 2.0.0 — 2026-09-25

The biggest release so far: a new identity, a native macOS app, a page for
every project (commands, workers, packages, Git, terminal, domains), a
built-in terminal, PostgreSQL and MongoDB next to MySQL, backups, and a
Ports & SSL page.

### Highlights

- **New look.** New logo and app icon, and a redesigned "Midnight + coral"
  control panel with a dark mode and in-app dialogs in place of browser
  pop-ups.
- **Mullion.app on macOS.** Setup puts a native app in `/Applications`
  (`mullion app` reinstalls it). It uses real windows, and those windows
  merge into macOS window tabs. On Windows the panel opens in its own app
  window.
- **Sites as cards.** Mullion detects each project's framework and
  language. The page has search, filters, pinning, and project tabs.
- **A page for every project**, with Commands, Workers, Packages, Git,
  Terminal, Domains and Settings tabs.
- **A built-in terminal** with tabs and suggestions as you type. Each
  project's own PHP and Node versions are on `PATH`.
- **PostgreSQL and MongoDB**, alongside MySQL/MariaDB, each with its own
  admin tool (pgAdmin 4 and mongo-express). Also new: backups and restore.
- **Ports & SSL.** Shows every port Mullion uses and who holds it. You can
  change ports, manage the local certificate authority, and turn on
  wildcard DNS.

### Added

- **Sites page**
  - Cards detect the framework (Laravel, Symfony, WordPress, Drupal,
    CodeIgniter, Yii, CakePHP, Slim, Nuxt, Next.js, SvelteKit, Astro,
    Angular, Remix, Vue, React, Svelte, Vite, …) with its major version,
    the language, and the package manager.
  - Search by name, domain, path or framework; filter by Backend, Frontend
    or Pinned; pin sites to the top.
  - Copy path and Show in Finder/Explorer from each card. Cards also show
    the Git branch and a count of changes.
  - Project tabs keep several projects open side by side. A project can
    also open in its own window.
- **Project page**
  - **Commands** — artisan (Laravel), `bin/console` (Symfony), Composer
    and npm scripts. Commands run with live output, and you can mark
    favorites. Quick actions cover the common tasks.
  - **Workers** — supervised queue workers, the scheduler
    (`schedule:work`, no cron needed), Horizon, Reverb, Symfony Messenger,
    or any custom command.
    - A worker can start with the stack (autostart).
    - Crashed workers restart with backoff (1s, 2s, 4s… capped at a
      minute). After 5 crashes within 2 minutes a worker is marked
      crash-looping and retries stop.
    - Each worker has a log in `~/.mullion/logs`.
  - **Packages** — search Packagist and the npm registry, then install or
    remove packages. Both can go in as dev dependencies.
  - **Git**
    - Status, stage/unstage, discard, and a side-by-side diff.
    - Commit, amend, and "Commit & Push".
    - Fetch, then pull by merge, rebase or fast-forward only.
    - Push, publish a new branch, or push with `--force-with-lease`.
    - Branches (create, switch, delete), history, and stashes.
    - A banner appears during a merge or rebase with conflicts.
  - **Domains** — subdomain aliases (`api.shop.test`) and a wildcard
    (`*.shop.test`).
  - **Terminal** and **Settings** tabs.
- **Terminal** (xterm.js)
  - Tabs.
  - Suggestions for paths, commands, npm and Composer scripts, artisan commands
    and Git branches; Tab or Enter accepts one.
  - The project's own PHP, Node and Composer are first on `PATH`.
  - Opens on the Terminal page, in a separate window, or on the project
    page, as chosen in Settings.
- **Databases**
  - Tabs for MySQL, PostgreSQL and MongoDB. Each engine installs, starts
    and stops on its own, and a stopped engine stays stopped.
  - Create and drop databases.
  - Backups: back up now, restore, delete, and choose a backup folder.
  - Uninstalling an engine offers a backup first.
  - Admin tools (phpMyAdmin, pgAdmin 4, mongo-express) sit in the top bar
    and can be uninstalled.
- **PHP**
  - Extension toggles: all extensions on Windows; OPcache and APCu on
    macOS, where builds are static.
  - A php.ini editor for the common settings (memory and upload limits,
    execution time, errors, timezone, OPcache timestamps).
  - On macOS, the LDAP extension is built automatically when a version is
    installed. This needs the Command Line Tools.
  - Uninstall a version, and install or update Composer.
- **Node** — choose the npm version for each Node install, uninstall
  versions, and view diagnostics.
- **Ports & SSL**
  - Every port Mullion uses and which process holds it.
  - Move database or dev-server ports, with conflict detection.
  - Trust or export the local CA, and turn HTTPS on or off for every site.
  - Wildcard DNS: a built-in DNS server plus `/etc/resolver` (macOS) or an
    NRPT rule (Windows).
- **Settings** — change the TLD, choose where terminals open, set the
  backup folder, and run diagnostics (doctor).
- **New commands:** `postgres` (`pg`), `mongo`, `pgadmin`, `mongo-express`,
  `phpmyadmin uninstall`, `backups`, `alias`, `dns`, `ports`, `port set`,
  `port dev`, `ssl` (`trust`, `export`, `all`), `worker`, `run`, `pkg`,
  `php ini`, `app`.

### Changed

- Rebranded panel, app icon, Windows `.exe` icon and favicon.
- Panel preferences (theme, open project tabs, terminal placement, favorite
  commands, …) are now stored in `config.json` under `ui` instead of the
  browser's storage.
- The sidebar has new pages: Sites (backend and frontend together),
  Terminal, and Ports & SSL. The old separate Backend and Frontend pages
  are gone.
- Managed dev servers and a project's commands now trust Mullion's local
  CA (`NODE_EXTRA_CA_CERTS`), so server-side calls to other
  `https://*.test` sites work.

### Fixed

- New domains added from the panel or the wake agent work right away.
  Background processes were mistaken for an interactive terminal, so the
  hosts-file update asked for a `sudo` password that nobody could type.
  Mullion now detects a real terminal and otherwise shows the system
  password dialog.
- Stopped the bursts of 502 errors on Vite pages that load many modules
  at once (`max_conns_per_host` on the dev-server proxy).
- Config files are written atomically, so a crash in the middle of a
  write can no longer corrupt `config.json` or `sites.json`.
- Stopping a database engine now sticks: Mullion no longer restarts an
  engine you stopped on purpose.

### Notes for upgrading

- Run `mullion setup` once after upgrading (it's idempotent). On macOS
  this installs Mullion.app into `/Applications`.
- Panel preferences start fresh once. They now live in `~/.mullion/config.json`
  (`C:\Mullion\config.json` on Windows) instead of the browser's storage.
  The reason: the panel runs on a random `127.0.0.1` port each launch, so
  anything the browser saved for one launch was lost on the next.
- Wildcard aliases (`*.site.test`) need `mullion dns on` once, which asks
  for administrator rights. Named aliases (`api.site.test`) keep using the
  hosts file.
- Existing sites, PHP/Node versions and MySQL data carry over unchanged.

## 1.x (August 2026)

- **1.7.0 – 1.7.3**
  - Wake-on-demand dev servers: opening a stopped frontend site starts it,
    with a live "Starting…" page.
  - Idle sleep: after 2 minutes with no tab open, or 10 minutes with an
    idle tab.
  - Startup failures show their real cause on the page.
  - Reliability fixes for the wake agent and Caddy reloads.
- **1.6.0**
  - Redesigned panel: sidebar pages (Overview, Backend, Frontend, PHP,
    Node, Database) and light/dark themes.
  - Rename sites from the panel and with `mullion rename`.
- **1.5.0 – 1.5.10**
  - Start, stop and restart each dev server.
  - Switch a domain between dev and build (`mullion serve`), with an
    automatic build when there is none yet.
  - Added `mullion doctor` and `mullion node which`.
  - Replacing another MySQL always offers a backup first.
  - Vite host fixes, and many self-healing fixes.
- **1.4.0 – 1.4.1** — Node.js support:
  - Version manager, per-project versions through `.nvmrc`-aware shims.
  - Managed dev servers behind `.test` domains.
  - `--build` links that serve a production build.
- **1.3.0**
  - MySQL root password and database management.
  - Automatic fixes for PHP versions shadowing Mullion's on `PATH`.
- **1.2.0 – 1.2.4**
  - macOS support: static-php builds, php-fpm, launchd, `/etc/hosts`.
  - Homebrew tap, Scoop bucket, and an install script.
- **1.1.0**
  - Windows tray icon, PECL extensions, and in-place self-update.
  - Fixes for large backups.
- **1.0.0**
  - First release: a PHP version manager and local dev server for Windows.
  - Caddy, `.test` domains, HTTPS, MySQL and phpMyAdmin.
