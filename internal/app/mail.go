package app

// Mail: Mullion's local mail catcher (Mailpit). The server itself lives
// in internal/mailpit; this file wires it into the stack — Apply's
// self-heal, the mail.<tld> Caddy block and hosts entry, the ports, the
// "Catch PHP mail()" php.ini switch, and doctor.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"pm/internal/caddy"
	"pm/internal/fcgi"
	"pm/internal/mailpit"
	"pm/internal/phpver"
)

var errMailNotInstalled = errors.New("Mail is not installed (run: mullion mail install)")

// MailHost is the inbox's hostname: mail.<tld>.
func MailHost(tld string) string { return mailpit.SiteName + "." + tld }

// MailURL is the inbox in the browser.
func (a *App) MailURL() string { return "https://" + MailHost(a.State.Config.TLD) }

// mailSiteActive reports whether mail.<tld> is served for the inbox:
// Mail is installed and no linked site already took the name "mail".
func (a *App) mailSiteActive() bool {
	return a.State.Config.Mailpit != "" && a.State.FindSite(mailpit.SiteName) == nil
}

// mailSiteConf is the Caddy block that proxies mail.<tld> to the inbox
// (Caddy's reverse_proxy carries Mailpit's live-update websocket as is).
func (a *App) mailSiteConf() caddy.SiteConf {
	return caddy.SiteConf{
		Name:      mailpit.SiteName,
		Host:      MailHost(a.State.Config.TLD),
		Kind:      "proxy",
		ProxyPort: mailpit.UIPort,
		Secure:    true,
	}
}

// InstallMail downloads the latest Mailpit (a no-op download when that
// version is already there), makes it the active one, starts it, and
// links https://mail.<tld>. Run again later it is the upgrade: the old
// version is stopped and removed once the new one is up. Returns the
// version now active.
func (a *App) InstallMail(ctx context.Context, progress mailpit.Progress) (string, error) {
	v, err := mailpit.Install(ctx, a.Paths, progress)
	if err != nil {
		return "", err
	}
	return v, a.activateMail(v)
}

// activateMail switches the config to an installed version and brings it
// up (see InstallMail).
func (a *App) activateMail(v string) error {
	c := &a.State.Config
	prev := c.Mailpit
	if prev != "" && prev != v {
		if err := mailpit.Stop(a.Paths); err != nil {
			return err
		}
	}
	c.Mailpit = v
	c.MailStopped = false
	if err := a.State.Save(); err != nil {
		return err
	}
	if !a.skipApply {
		if err := mailpit.Start(a.Paths, v); err != nil {
			return err
		}
	}
	if prev != "" && prev != v {
		_ = mailpit.RemoveVersion(a.Paths, prev)
	}
	// sendmail_path names the versioned binary: follow the switch.
	a.restartPHPVersions(a.syncMailIni())
	return a.applyIfEnabled()
}

// MailUpdate reports the newest published Mailpit when it is newer than
// the installed one ("" otherwise, or when GitHub can't be reached).
func (a *App) MailUpdate(ctx context.Context) string {
	cur := a.State.Config.Mailpit
	if cur == "" {
		return ""
	}
	latest, err := mailpit.LatestVersion(ctx)
	if err != nil || !mailpit.Newer(latest, cur) {
		return ""
	}
	return latest
}

// StartMail starts Mailpit and clears the stopped flag, so self-heal
// keeps it up from now on.
func (a *App) StartMail() error {
	v := a.State.Config.Mailpit
	if v == "" {
		return errMailNotInstalled
	}
	if err := mailpit.Start(a.Paths, v); err != nil {
		return err
	}
	a.State.Config.MailStopped = false
	return a.State.Save()
}

// StopMail stops Mailpit and records that the user did, so
// Apply/self-heal/autostart leave it down until StartMail.
func (a *App) StopMail() error {
	if a.State.Config.Mailpit == "" {
		return nil
	}
	if err := mailpit.Stop(a.Paths); err != nil {
		return err
	}
	a.State.Config.MailStopped = true
	return a.State.Save()
}

// RestartMail bounces Mailpit (e.g. after the relay settings changed).
func (a *App) RestartMail() error {
	if a.State.Config.Mailpit == "" {
		return errMailNotInstalled
	}
	if err := mailpit.Stop(a.Paths); err != nil {
		return err
	}
	return a.StartMail()
}

// UninstallMail stops Mailpit, puts php.ini's mail settings back,
// deletes the binaries (and the caught messages unless keepMail), and
// unlinks mail.<tld>. The port settings are kept.
func (a *App) UninstallMail(keepMail bool) error {
	c := &a.State.Config
	if err := mailpit.Stop(a.Paths); err != nil {
		return err
	}
	if c.MailCatchPHP || len(c.MailIniPrev) > 0 {
		a.restartPHPVersions(a.restoreMailIni())
	}
	c.MailCatchPHP = false
	if err := mailpit.Remove(a.Paths, keepMail); err != nil {
		return err
	}
	c.Mailpit = ""
	c.MailStopped = false
	return a.applyIfEnabled()
}

// SaveMailRelay stores the "release to a real inbox" relay and restarts
// a running Mailpit so it takes effect.
func (a *App) SaveMailRelay(cfg mailpit.RelayConfig) error {
	if err := mailpit.SaveRelay(a.Paths, cfg); err != nil {
		return err
	}
	if a.State.Config.Mailpit == "" || a.State.Config.MailStopped || a.skipApply || !mailpit.Owned(a.Paths) {
		return nil
	}
	return a.RestartMail()
}

/* ── php.ini: Catch PHP mail() ─────────────────────────────── */

// mailIniDirectives is what "Catch PHP mail()" sets. On macOS/Linux,
// mail() pipes the message into sendmail_path — Mailpit's own sendmail
// subcommand. On Windows PHP ignores sendmail_path and speaks SMTP
// itself, to SMTP:smtp_port.
func mailIniDirectives(goos, exe string, port int) map[string]string {
	if goos == "windows" {
		return map[string]string{"SMTP": "127.0.0.1", "smtp_port": strconv.Itoa(port)}
	}
	return map[string]string{"sendmail_path": `"` + mailpit.SendmailCommand(exe, port) + `"`}
}

// catchMailIni sets the directives in one php.ini. The values they
// replace are captured the first time (recorded=false) and returned, so
// restoreMailIni can put them back; later calls keep the original
// capture. changed reports whether the file was modified.
func catchMailIni(path string, wants, prev map[string]string, recorded bool) (map[string]string, bool, error) {
	if !recorded {
		prev = map[string]string{}
		for k := range wants {
			if v, ok := phpver.IniDirective(path, k); ok {
				prev[k] = v
			}
		}
	}
	changed := false
	for _, k := range sortedKeys(wants) {
		if cur, ok := phpver.IniDirective(path, k); ok && cur == wants[k] {
			continue
		}
		if err := phpver.SetIniDirective(path, k, wants[k]); err != nil {
			return prev, changed, err
		}
		changed = true
	}
	return prev, changed, nil
}

// uncatchMailIni puts one php.ini's directives back to the captured
// values, removing the ones that weren't set before.
func uncatchMailIni(path string, keys []string, prev map[string]string) (bool, error) {
	changed := false
	for _, k := range keys {
		cur, ok := phpver.IniDirective(path, k)
		if old, had := prev[k]; had {
			if ok && cur == old {
				continue
			}
			if err := phpver.SetIniDirective(path, k, old); err != nil {
				return changed, err
			}
			changed = true
		} else if ok {
			if err := phpver.RemoveIniDirective(path, k); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// mailIniGOOS is the platform whose php.ini directives are managed — a
// seam for tests.
var mailIniGOOS = runtime.GOOS

// syncMailIni makes every installed PHP version's php.ini catch mail()
// while MailCatchPHP is on (new versions included — Apply runs this),
// and returns the versions whose php.ini changed.
func (a *App) syncMailIni() []string {
	c := &a.State.Config
	if !c.MailCatchPHP || c.Mailpit == "" {
		return nil
	}
	versions, _ := phpver.Installed(a.Paths)
	wants := mailIniDirectives(mailIniGOOS, mailpit.Exe(a.Paths, c.Mailpit), mailpit.SMTPPort)
	if c.MailIniPrev == nil {
		c.MailIniPrev = map[string]map[string]string{}
	}
	var changed []string
	dirty := false
	for _, v := range versions {
		path := phpver.IniPath(a.Paths, v)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		prev, recorded := c.MailIniPrev[v]
		prev, ch, err := catchMailIni(path, wants, prev, recorded)
		if err != nil {
			fmt.Printf("warning: PHP %s: could not set its mail settings: %v\n", v, err)
		}
		if !recorded {
			c.MailIniPrev[v] = prev
			dirty = true
		}
		if ch {
			changed = append(changed, v)
		}
	}
	// Forget versions that were uninstalled since.
	for v := range c.MailIniPrev {
		if !slices.Contains(versions, v) {
			delete(c.MailIniPrev, v)
			dirty = true
		}
	}
	if dirty {
		_ = a.State.Save()
	}
	return changed
}

// restoreMailIni undoes syncMailIni in every php.ini it touched, and
// returns the versions whose php.ini changed.
func (a *App) restoreMailIni() []string {
	c := &a.State.Config
	keys := sortedKeys(mailIniDirectives(mailIniGOOS, "", 0))
	var changed []string
	for v, prev := range c.MailIniPrev {
		ch, err := uncatchMailIni(phpver.IniPath(a.Paths, v), keys, prev)
		if err != nil {
			fmt.Printf("warning: PHP %s: could not restore its mail settings: %v\n", v, err)
			continue
		}
		if ch {
			changed = append(changed, v)
		}
	}
	sort.Strings(changed)
	c.MailIniPrev = nil
	_ = a.State.Save()
	return changed
}

// restartPHPVersions bounces the php-fpm/php-cgi workers of versions
// whose php.ini changed, so the change applies now.
func (a *App) restartPHPVersions(versions []string) {
	if a.skipApply {
		return
	}
	for _, v := range versions {
		if err := a.RestartPhp(v); err != nil {
			fmt.Printf("warning: restarting PHP %s: %v\n", v, err)
		}
	}
}

// SetMailCatchPHP turns "Catch PHP mail()" on or off, restarting the
// PHP workers whose php.ini changed. Turning it off restores the values
// it replaced.
func (a *App) SetMailCatchPHP(on bool) error {
	c := &a.State.Config
	if on && c.Mailpit == "" {
		return errMailNotInstalled
	}
	if on == c.MailCatchPHP && (on || len(c.MailIniPrev) == 0) {
		return nil
	}
	c.MailCatchPHP = on
	if err := a.State.Save(); err != nil {
		return err
	}
	if on {
		a.restartPHPVersions(a.syncMailIni())
	} else {
		a.restartPHPVersions(a.restoreMailIni())
	}
	return nil
}

// afterMailPortChange follows a moved Mail port: php.ini's mail()
// settings name the SMTP port, mail.<tld>'s Caddy block the inbox port.
func (a *App) afterMailPortChange(engine string) error {
	if engine == "mail-smtp" {
		a.restartPHPVersions(a.syncMailIni())
		return nil
	}
	return a.applyIfEnabled()
}

/* ── connection snippets ──────────────────────────────────── */

// MailEnvSnippet is the Laravel .env block for Mullion's Mail.
// MAIL_SCHEME is Laravel 11+'s name; MAIL_ENCRYPTION is 10 and older's —
// both are given, each version ignores the other.
func MailEnvSnippet(port int) string {
	return strings.Join([]string{
		"MAIL_MAILER=smtp",
		"MAIL_HOST=127.0.0.1",
		"MAIL_PORT=" + strconv.Itoa(port),
		"MAIL_USERNAME=null",
		"MAIL_PASSWORD=null",
		"MAIL_SCHEME=null",
		"MAIL_ENCRYPTION=null",
		`MAIL_FROM_ADDRESS="hello@example.test"`,
		`MAIL_FROM_NAME="${APP_NAME}"`,
	}, "\n") + "\n"
}

// MailNodeSnippet is a nodemailer transport for Mullion's Mail.
func MailNodeSnippet(port int) string {
	return `import nodemailer from 'nodemailer';

const transport = nodemailer.createTransport({
  host: '127.0.0.1',
  port: ` + strconv.Itoa(port) + `,
  secure: false, // plain SMTP — Mullion's Mail has no TLS
});

await transport.sendMail({
  from: 'hello@example.test',
  to: 'you@example.test',
  subject: 'Hello from Node',
  text: 'It works!',
});
`
}

/* ── doctor ────────────────────────────────────────────────── */

func (a *App) mailDoctorChecks() []DoctorCheck {
	c := a.State.Config
	if c.Mailpit == "" {
		return nil
	}
	name := "mail " + c.Mailpit
	ours := mailpit.Owned(a.Paths)
	up := mailpit.Running() && mailpit.SMTPUp()
	var check DoctorCheck
	switch {
	case ours && up:
		check = DoctorCheck{Name: name, Status: DoctorOK,
			Detail: fmt.Sprintf("SMTP 127.0.0.1:%d, inbox %s", mailpit.SMTPPort, a.MailURL())}
	case !ours && (mailpit.Running() || mailpit.SMTPUp()):
		check = DoctorCheck{Name: name, Status: DoctorFail,
			Detail: fmt.Sprintf("another program holds port %d or %d — stop it, or move Mail's ports (`mullion ports`)", mailpit.SMTPPort, mailpit.UIPort)}
	case c.MailStopped:
		check = DoctorCheck{Name: name, Status: DoctorWarn, Detail: "stopped by you (`mullion mail start`)"}
	default:
		check = DoctorCheck{Name: name, Status: DoctorFail, Detail: "not running — `mullion mail start`"}
		if data, err := os.ReadFile(mailpit.LogFile(a.Paths)); err == nil && len(data) > 0 {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 4 {
				lines = lines[len(lines)-4:]
			}
			var b strings.Builder
			b.WriteString("  mailpit log tail:\n")
			for _, l := range lines {
				b.WriteString("    " + l + "\n")
			}
			check.Fix = b.String()
		}
	}
	checks := []DoctorCheck{check}
	if c.MailCatchPHP {
		checks = append(checks, DoctorCheck{Name: "mail: php mail()", Status: DoctorOK, Detail: "caught by Mail"})
	}
	if a.State.FindSite(mailpit.SiteName) != nil {
		checks = append(checks, DoctorCheck{Name: "mail: inbox", Status: DoctorWarn,
			Detail: "a linked site is named \"mail\", so the inbox isn't on " + MailHost(c.TLD) + fmt.Sprintf(" — use http://127.0.0.1:%d", mailpit.UIPort)})
	}
	return checks
}

// stopMailForStack stops Mailpit for a whole-stack stop without setting
// the user's stopped flag (like the databases).
func (a *App) stopMailForStack() error {
	if a.State.Config.Mailpit == "" {
		return nil
	}
	return mailpit.Stop(a.Paths)
}

// healMail is Apply's self-heal for Mail. A failure is only reported:
// a port taken by another mail catcher must not fail every command.
func (a *App) healMail() {
	c := a.State.Config
	if !shouldSelfHeal(c.Mailpit, c.MailStopped) {
		return
	}
	if err := mailpit.Start(a.Paths, c.Mailpit); err != nil {
		fmt.Println("warning: Mail:", err)
	}
}

// stopFcgiForIni makes Apply's fcgi.Ensure restart workers whose php.ini
// syncMailIni just changed.
func (a *App) stopFcgiForIni() {
	for _, v := range a.syncMailIni() {
		_ = fcgi.StopVersion(a.Paths, v)
	}
}
