package app

// Updating an installed PHP version to the newest patch of its branch
// (8.4.23 -> 8.4.27): install the new patch the normal way (macOS
// rebuilds ldap/pdo_dblib for it, Windows fetches sqlsrv), carry the
// php.ini over (phpver.MigrateIni), check the new PHP starts cleanly,
// and only then switch everything that ran on the old patch — the
// global version (the `current` link, so php/composer on PATH), sites
// pinned to it, their workers — restart the FastCGI pool and, unless
// asked to keep it, remove the old patch. Anything failing before the
// switch leaves the old version exactly as it was.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"pm/internal/fcgi"
	"pm/internal/phpver"
	"pm/internal/workers"
)

// ErrPHPUpToDate is returned by UpdatePHP when no newer patch exists.
var ErrPHPUpToDate = errors.New("already the newest patch release")

// Seams for tests: resolving, installing and smoke-testing a release
// would otherwise download PHP and run it.
var (
	phpResolve = phpver.Resolve
	phpInstall = phpver.Install
	phpVerify  = phpver.VerifyInstall
	phpUpdates = phpver.Updates
)

// PHPUpdateOptions tunes UpdatePHP.
type PHPUpdateOptions struct {
	// KeepOld keeps the old patch installed after the switch (default:
	// remove it).
	KeepOld bool
	// Log receives one line per step (the panel's job log, the CLI).
	Log func(string)
}

func (o PHPUpdateOptions) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(format, args...))
	}
}

// PHPUpdatePlan is what switching from one patch to another touches.
type PHPUpdatePlan struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Security bool   `json:"security"`
	// Global: From is the global version (php on PATH, composer, and
	// every site that follows the global version).
	Global bool `json:"global"`
	// Sites pinned (isolated) to From.
	Sites []string `json:"sites"`
	// Following counts the PHP sites that follow the global version —
	// they move along when Global.
	Following int `json:"following"`
	// Workers counts the background workers of the affected sites.
	Workers int `json:"workers"`
	// TargetInstalled: To is already installed (its own php.ini is kept).
	TargetInstalled bool `json:"targetInstalled"`
}

// PlanPHPUpdate describes switching from -> to without changing anything.
func (a *App) PlanPHPUpdate(from, to string) PHPUpdatePlan {
	p := PHPUpdatePlan{From: from, To: to, Global: a.State.Config.GlobalPHP == from, Sites: []string{}}
	for _, s := range a.State.Sites {
		switch {
		case s.PHP == from:
			p.Sites = append(p.Sites, s.Name)
			p.Workers += len(s.Workers)
		case s.PHP == "" && p.Global:
			if s.IsPHP() {
				p.Following++
			}
			p.Workers += len(s.Workers)
		}
	}
	sort.Strings(p.Sites)
	if installed, err := phpver.Installed(a.Paths); err == nil {
		for _, v := range installed {
			if v == to {
				p.TargetInstalled = true
			}
		}
	}
	return p
}

// PHPUpdates reports the available patch updates of every installed
// version (see phpver.Updates; force skips its 6-hour cache).
func (a *App) PHPUpdates(ctx context.Context, force bool) (map[string]phpver.Update, error) {
	installed, err := phpver.Installed(a.Paths)
	if err != nil {
		return nil, err
	}
	return phpUpdates(ctx, a.Paths, installed, force)
}

// PHPUpdateResult reports one finished update.
type PHPUpdateResult struct {
	PHPUpdatePlan
	Migration phpver.MigrateReport `json:"migration"`
	// Removed: the old patch was deleted.
	Removed bool `json:"removed"`
	// Warnings are non-fatal problems (the update itself went through).
	Warnings []string `json:"warnings,omitempty"`
}

// UpdatePHP updates the installed version matching version ("8.4.23",
// or "8.4" for the newest installed 8.4.x) to the newest patch of its
// branch. See the file comment for the steps and their guarantees.
func (a *App) UpdatePHP(ctx context.Context, version string, opts PHPUpdateOptions) (PHPUpdateResult, error) {
	var res PHPUpdateResult
	sel, err := phpver.ParseSelector(version)
	if err != nil {
		return res, err
	}
	if sel.Minor == -1 {
		return res, fmt.Errorf("say which branch to update, e.g. %d.4 (an update stays within one major.minor)", sel.Major)
	}
	from, err := phpver.FindInstalled(a.Paths, sel)
	if err != nil {
		return res, err
	}
	fromSel, _ := phpver.ParseSelector(from)

	opts.log("Looking up the newest PHP %d.%d release on %s…", fromSel.Major, fromSel.Minor, phpver.Source)
	rel, err := phpResolve(ctx, phpver.Selector{Major: fromSel.Major, Minor: fromSel.Minor, Patch: -1})
	if err != nil {
		return res, err
	}
	if phpver.Compare(rel.Version, from) <= 0 {
		return res, fmt.Errorf("PHP %s: %w", from, ErrPHPUpToDate)
	}
	to := rel.Version
	res.PHPUpdatePlan = a.PlanPHPUpdate(from, to)
	if ups, err := phpUpdates(ctx, a.Paths, []string{from}, false); err == nil {
		res.Security = ups[from].To != "" && ups[from].Security
	}
	newDir := a.Paths.PhpVersionDir(to)
	preexisting := res.TargetInstalled
	// cleanup undoes a fresh install that won't be switched to.
	cleanup := func() {
		if !preexisting {
			_ = os.RemoveAll(newDir)
		}
	}

	// 1. Install (no-op when it already is).
	opts.log("Installing PHP %s (download%s)…", to, buildNote())
	if _, err := phpInstall(ctx, a.Paths, rel); err != nil {
		cleanup()
		return res, fmt.Errorf("installing PHP %s: %w — PHP %s is untouched", to, err, from)
	}

	// 2. Carry the configuration over — into a fresh install only: an
	// already installed target keeps the php.ini someone set up for it.
	if preexisting {
		res.Warnings = append(res.Warnings, fmt.Sprintf("PHP %s was already installed — its own php.ini is kept (not copied from %s)", to, from))
	} else {
		opts.log("Carrying php.ini over from PHP %s…", from)
		rep, err := phpver.MigrateIni(a.Paths, from, to)
		res.Migration = rep
		if err != nil {
			cleanup()
			return res, fmt.Errorf("carrying php.ini over: %w — PHP %s is untouched", err, from)
		}
		if rep.Repointed > 0 {
			opts.log("  %d php.ini line(s) re-pointed from %s to %s", rep.Repointed, from, to)
		}
		for _, c := range rep.Copied {
			opts.log("  carried over %s", c)
		}
		for _, d := range rep.Disabled {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s is not available for PHP %s yet, so it was switched off — add it with: mullion php ext get %s %s", d, to, d, to))
		}
		for _, n := range rep.Added {
			opts.log("  enabled %s (new in this install)", n)
		}
	}

	// 3. The new PHP must start cleanly before anything points at it.
	opts.log("Checking PHP %s starts with its php.ini…", to)
	if err := phpVerify(a.Paths, to); err != nil {
		cleanup()
		return res, fmt.Errorf("%w — not switching; PHP %s is untouched", err, from)
	}

	// 4. Switch.
	c := &a.State.Config
	if res.Global {
		opts.log("Switching the global PHP to %s…", to)
		if err := a.UseGlobal(to); err != nil {
			cleanup()
			return res, fmt.Errorf("switching the global PHP: %w — PHP %s is untouched", err, from)
		}
	}
	for i := range a.State.Sites {
		if a.State.Sites[i].PHP == from {
			a.State.Sites[i].PHP = to
		}
	}
	if len(res.Sites) > 0 {
		opts.log("Moving %d pinned site(s) to %s: %v", len(res.Sites), to, res.Sites)
	}
	// "Catch PHP mail()" remembers per version what it replaced; the
	// carried-over php.ini has the same lines, so the memory moves too.
	if prev, ok := c.MailIniPrev[from]; ok && !preexisting {
		c.MailIniPrev[to] = prev
	}
	if err := a.State.Save(); err != nil {
		return res, err
	}

	// php-fpm/php-cgi listen on a per-branch port: the old patch's pool
	// must go before Apply can start the new one there.
	if !a.skipApply {
		_ = fcgi.StopVersion(a.Paths, from)
	}
	opts.log("Restarting PHP and the sites…")
	applyErr := a.applyIfEnabled()
	a.restartSiteWorkers(res.PHPUpdatePlan, opts, &res)
	if applyErr != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("restarting services: %v — PHP %s was kept", applyErr, from))
		return res, nil
	}

	// 5. Remove the old patch.
	if opts.KeepOld {
		opts.log("Keeping PHP %s installed (remove it later with: mullion php uninstall %s)", from, from)
		return res, nil
	}
	opts.log("Removing PHP %s…", from)
	if err := removeAllRetry(a.Paths.PhpVersionDir(from)); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("PHP %s could not be removed (%v) — remove it later with: mullion php uninstall %s", from, err, from))
	} else {
		res.Removed = true
		if _, ok := c.MailIniPrev[from]; ok {
			delete(c.MailIniPrev, from)
			_ = a.State.Save()
		}
	}
	return res, nil
}

// buildNote is the platform's remark for the install step.
func buildNote() string {
	if phpver.Source == "windows.php.net" {
		return ""
	}
	return "; on macOS LDAP and SQL Server modules are rebuilt for it, a few minutes"
}

// restartSiteWorkers restarts the running background workers of the
// sites whose PHP just changed, so they stop running the old binary.
func (a *App) restartSiteWorkers(p PHPUpdatePlan, opts PHPUpdateOptions, res *PHPUpdateResult) {
	if a.skipApply || p.Workers == 0 {
		return
	}
	for _, s := range a.State.Sites {
		affected := contains(p.Sites, s.Name) || (p.Global && s.PHP == "")
		if !affected {
			continue
		}
		for _, w := range s.Workers {
			if !workers.Running(a.Paths, s.Name, w.ID) {
				continue
			}
			opts.log("Restarting worker %s/%s…", s.Name, w.Name)
			if err := a.RestartWorker(s.Name, w.ID); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("restarting worker %s/%s: %v", s.Name, w.Name, err))
			}
		}
	}
}

// removeAllRetry removes a directory, retrying briefly: on Windows a
// just-killed php-cgi can hold its files for a moment.
func removeAllRetry(dir string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return err
}

// UpdateAllPHP updates every installed version that has a newer patch,
// oldest first. It stops at the first failure (earlier updates stay).
func (a *App) UpdateAllPHP(ctx context.Context, opts PHPUpdateOptions) ([]PHPUpdateResult, error) {
	ups, err := a.PHPUpdates(ctx, true)
	if err != nil {
		return nil, err
	}
	froms := make([]string, 0, len(ups))
	for v := range ups {
		froms = append(froms, v)
	}
	sort.Slice(froms, func(i, j int) bool { return phpver.Compare(froms[i], froms[j]) < 0 })
	var out []PHPUpdateResult
	for _, v := range froms {
		res, err := a.UpdatePHP(ctx, v, opts)
		if errors.Is(err, ErrPHPUpToDate) {
			continue
		}
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}
