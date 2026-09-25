# Mullion — a complete local dev server for Windows

**Mullion** is a local PHP development environment for Windows, shipped as a
**single file** (`mullion.exe`) with no dependencies. It installs and manages
**PHP** (several versions side by side), **Composer**, **MySQL/MariaDB**, and
**phpMyAdmin/HeidiSQL**, and serves your projects on local domains like
`http://myapp.test` — with one-command **HTTPS** using locally-trusted
certificates. Everything can be driven from a desktop control panel or from
the terminal.

**Requirements:** Windows 10 or 11, 64-bit. That's it — setup installs
everything else automatically (including the Microsoft VC++ runtime if the
machine doesn't have it).

---

## Installation

1. Copy `mullion.exe` anywhere on the machine.
2. **Double-click it** → it asks `Run setup now? [Y/n]` → press Enter.
3. Accept the **UAC prompt** (once — the whole setup continues with admin
   rights in a new window, so there are no further prompts).
4. It asks `Start Mullion automatically when you sign in to Windows? [Y/n]` —
   press Enter if you want the services to come up with Windows.
   Then it asks which **database manager** you want: phpMyAdmin (browser),
   HeidiSQL (desktop app), both, or none — and installs accordingly.
5. Wait for the downloads (~400MB): latest PHP + Composer + MySQL +
   phpMyAdmin + the Caddy web server.

When it finishes, open a **new** terminal: `mullion`, `php`, and `composer` work
from any directory, and phpMyAdmin is live at `https://phpmyadmin.test`.
A **Mullion shortcut appears on your Desktop** — it opens the control panel
and points at the installed copy, so you can safely delete the file you
downloaded.

> Re-running setup is always safe — every step that is already done is skipped.

**Already have Laragon / XAMPP?** Setup handles it: Mullion registers itself
*ahead* of them on the system PATH (so `php` and `composer` always mean
Mullion's), and if their servers are sitting on ports 80/443/3306, setup
lists them and offers to stop them so Mullion becomes the main stack.
If their MySQL is running, setup also offers to **import its databases
into Mullion automatically** (they're exported to `C:\Mullion-Backups`
first). Nothing of theirs is deleted — their apps and data stay untouched.

---

## The control panel

After setup, **double-clicking `mullion.exe`** opens the **control panel** as a
regular desktop window (or run `mullion ui` from a terminal). From the panel you can:

- Start / stop all services with one button
- Switch the global PHP version and install new versions
- Start / stop MySQL and open phpMyAdmin
- Link a new project by pasting its folder path, enable HTTPS per site,
  pin a PHP version per site, and unlink sites
- Toggle "Start with Windows"

---

## Commands in detail

### Setup & autostart

| Command | What it does |
|---|---|
| `mullion setup` | Full first-time setup: creates `C:\Mullion`, installs the VC++ runtime if missing, downloads Caddy, copies mullion.exe and adds it to your PATH, installs the latest PHP and makes it the system default, installs Composer and the latest MySQL (initialized and running), and serves phpMyAdmin over HTTPS. Asks for admin rights once and asks about autostart. |
| `mullion ui` | Opens the control panel in its own window. |
| `mullion autostart` | Shows whether Mullion starts automatically at sign-in. |
| `mullion autostart on` | Services start automatically when you sign in to Windows. |
| `mullion autostart off` | Disables autostart. |
| `mullion tray` | Puts a Mullion icon in the notification area (next to the clock): double-click opens the dashboard; right-click has Start/Stop, phpMyAdmin, Quit. Runs automatically at sign-in and after setup. |
| `mullion uninstall` | Removes Mullion completely — services, PHP, MySQL, phpMyAdmin, Composer, hosts/PATH entries, certificate, autostart. First offers to back your databases up to `C:\Mullion-Backups\<date>\` — one `.sql` per database plus `all-databases.sql` (backups survive the uninstall). **Project folders are never touched.** |

### Managing PHP versions

| Command | What it does |
|---|---|
| `mullion php available` | Lists the versions installable from windows.php.net. |
| `mullion php install 8.3` | Installs the newest 8.3.x release (or pin it exactly: `mullion php install 8.3.30`). Works even for end-of-life versions like `7.4`. |
| `mullion php list` | Lists installed versions (`*` marks the active global one). |
| `mullion php uninstall 8.3` | Removes an installed version (refuses if it's the global one or pinned to a site). |
| `mullion use 8.3` | Switches the **system-wide** PHP version — `php -v` in any new terminal picks it up instantly, no admin rights needed. |
| `mullion php ext list [v]` | Lists a version's extensions (`[x]` = enabled); defaults to the global version. |
| `mullion php ext enable soap [v]` | Enables an extension (e.g. `soap`, `intl`, `ldap`) and restarts that version's php-cgi so sites pick it up immediately. |
| `mullion php ext disable soap [v]` | Disables an extension. Also available in the control panel: PHP card → **Extensions → Manage**. |
| `mullion php ext get redis [v]` | Downloads a **PECL** extension the stock build doesn't ship (redis, xdebug, imagick, ...), matched to the exact PHP build, and enables it. |

### A different PHP version per project

| Command | What it does |
|---|---|
| `mullion isolate 7.4` | (inside the project folder) Pins this site to PHP 7.4 even while the rest of the machine runs a newer version — both run **at the same time**, since every version gets its own php-cgi process on its own port. From elsewhere: `mullion isolate 7.4 --site myapp`. |
| `mullion unisolate` | The site follows the global version again. |

### Sites & domains

| Command | What it does |
|---|---|
| `mullion link` | (inside the project folder) Serves it at `http://<folder-name>.test`. Laravel-style projects are served from `public/` automatically. Pick a name: `mullion link myapp`. |
| `mullion unlink` | Stops serving the site (inside the folder, or `mullion unlink myapp`). |
| `mullion links` | Lists every linked site and the PHP version it runs on. |
| `mullion secure` | Switches the site to **HTTPS** with a locally-trusted certificate — the browser shows a proper padlock, and HTTP redirects automatically. |
| `mullion unsecure` | Back to plain HTTP. |
| `mullion tld` | Shows the current domain suffix (default `.test`). |
| `mullion tld local` | Changes it — every site becomes `<name>.local`. (Avoid `.dev` and `.app`: browsers force HTTPS on them.) |

### MySQL

| Command | What it does |
|---|---|
| `mullion mysql install` | Downloads the newest **8.4 LTS** MySQL, initializes it, and starts it on `127.0.0.1:3306` (user `root`, empty password). |
| `mullion mysql install latest` | The newest release overall (innovation branch). |
| `mullion mysql install 8.0` / `8.4.11` | A specific branch or exact version. |
| *switching versions* | Setup asks whether to **migrate your databases** to the new version: they're dumped from the old server and restored into the new one. A full backup lands in `C:\Mullion-Backups` first, and the old data directory is kept aside. |
| `mullion mysql restore <path>` | Imports a backup: pass one `.sql` file for a single database, or a whole backup folder. |
| `mullion mysql start` / `mullion mysql stop` | Starts / stops the server. |
| `mullion mysql uninstall` | Removes the current version's binaries (databases stay in `C:\Mullion\mysql\data`). |

### Composer & phpMyAdmin

| Command | What it does |
|---|---|
| `mullion composer install` | Installs the latest Composer — after that, `composer` works in any terminal and always runs on the active PHP version. |
| `mullion composer install 2.2.25` | Installs a specific Composer version (handy for legacy projects). |
| `mullion phpmyadmin` | Installs the latest phpMyAdmin and serves it, secured, at `https://phpmyadmin.test`, auto-connected to the local MySQL — no login screen. |
| `mullion phpmyadmin 5.2.2` | Installs a specific phpMyAdmin version. |
| `mullion heidisql` | Installs (first use) and opens the portable HeidiSQL desktop client, preconfigured with a `Mullion` session for the local MySQL. Also a button in the control panel. |

### Services

| Command | What it does |
|---|---|
| `mullion start` | Starts everything: Caddy + the needed php-cgi processes + MySQL. |
| `mullion stop` | Stops everything. |
| `mullion restart` | Stop, then start. |
| `mullion status` | Shows each service: Caddy, the global PHP version, every php-cgi with its port, MySQL, and the linked-site count — plus a warning if another PHP install shadows Mullion on your PATH. |

> Services run fully detached from the terminal — close the window, the sites
> keep working. If any service ever dies, the next mullion command heals it
> automatically.

---

## Where everything lives

Everything is under `C:\Mullion`:

| Path | Contents |
|---|---|
| `C:\Mullion\bin` | mullion.exe, caddy.exe, composer |
| `C:\Mullion\php\<version>` | PHP versions (`current` is a link to the global one) |
| `C:\Mullion\mysql` | MySQL binaries and your databases (`data`) |
| `C:\Mullion\phpmyadmin` | phpMyAdmin |
| `C:\Mullion\logs` | Logs for Caddy, PHP, MySQL, and every site |
| `C:\Mullion\Caddyfile`, `sites.json`, `config.json` | Configuration (auto-generated) |
| `C:\Mullion-Backups\<date>\` | Database backups: one `.sql` per database + `all-databases.sql`. Survives uninstall. Import with `mullion mysql restore` or from phpMyAdmin. |

## Troubleshooting

- **`php -v` shows the wrong version?** Another PHP (Laragon, XAMPP, ...) sits
  earlier on the system PATH — `mullion use` and `mullion status` detect this and print
  the exact offending path so you can remove it.
- **A site doesn't open?** Check `mullion status`; `mullion start` brings everything
  back. Logs are in `C:\Mullion\logs`.
- **Browser warns about the certificate?** Run `mullion setup` once more as
  administrator so the root certificate gets registered.
- **Nothing works after a reboot?** Run `mullion status` — if the command
  itself is missing, your antivirus probably quarantined
  `C:\Mullion\bin\mullion.exe` (see below). Otherwise `mullion start`
  brings everything up, and `mullion autostart on` makes it automatic.
- **The antivirus flags mullion.exe.** This is a false positive — new,
  unsigned executables are routinely flagged by heuristics. Restore the
  file from quarantine and add an exclusion for `C:\Mullion`
  (Windows Security → Virus & threat protection → Manage settings →
  Exclusions). On first run, SmartScreen may show "Windows protected your
  PC" — click **More info → Run anyway**. If you distribute Mullion,
  the permanent fix is signing the binary with an Authenticode
  certificate and submitting a false-positive report to your AV vendor
  (for Defender: https://www.microsoft.com/wdsi/filesubmission).