package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/config"
	"pm/internal/mailpit"
	"pm/internal/pmdir"
)

// mailTestApp is an App over a sandbox home with Mail "installed" (the
// config says so; no binary runs) and PHP versions with a php.ini each.
func mailTestApp(t *testing.T, phpInis map[string]string) *App {
	t.Helper()
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	for v, ini := range phpInis {
		dir := paths.PhpVersionDir(v)
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, "php.ini"), []byte(ini), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	state, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	state.Config.Mailpit = "1.31.6"
	oldSMTP, oldUI, oldGOOS := mailpit.SMTPPort, mailpit.UIPort, mailIniGOOS
	t.Cleanup(func() { mailpit.SMTPPort, mailpit.UIPort, mailIniGOOS = oldSMTP, oldUI, oldGOOS })
	mailpit.SMTPPort, mailpit.UIPort = mailpit.DefaultSMTPPort, mailpit.DefaultUIPort
	mailIniGOOS = "darwin"
	return &App{Paths: paths, State: state, skipApply: true}
}

func readIni(t *testing.T, a *App, v string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.Paths.PhpVersionDir(v), "php.ini"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMailIniDirectives(t *testing.T) {
	got := mailIniDirectives("darwin", "/h/.mullion/mailpit/1.31.6/mailpit", 1025)
	if len(got) != 1 || got["sendmail_path"] != `"/h/.mullion/mailpit/1.31.6/mailpit sendmail -S 127.0.0.1:1025"` {
		t.Errorf("darwin: %v", got)
	}
	got = mailIniDirectives("windows", `C:\Mullion\mailpit\1.31.6\mailpit.exe`, 2525)
	if len(got) != 2 || got["SMTP"] != "127.0.0.1" || got["smtp_port"] != "2525" {
		t.Errorf("windows: %v", got)
	}
}

func TestCatchPHPMailOnOffRestores(t *testing.T) {
	const custom = "[mail function]\n;sendmail_path =\nsendmail_path = \"/usr/sbin/sendmail -t -i\"\nmemory_limit=256M\n"
	const stock = "[mail function]\n;sendmail_path =\nmemory_limit=128M\n"
	a := mailTestApp(t, map[string]string{"8.4.1": custom, "8.3.9": stock})
	exe := mailpit.Exe(a.Paths, "1.31.6")
	want := `sendmail_path="` + exe + ` sendmail -S 127.0.0.1:1025"`

	if err := a.SetMailCatchPHP(true); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"8.4.1", "8.3.9"} {
		ini := readIni(t, a, v)
		if !strings.Contains(ini, want) || strings.Count(ini, "\nsendmail_path") != 1 {
			t.Errorf("PHP %s after on:\n%s", v, ini)
		}
		if !strings.Contains(ini, ";sendmail_path =") {
			t.Errorf("PHP %s: the commented default must stay", v)
		}
	}
	if prev := a.State.Config.MailIniPrev["8.4.1"]; prev["sendmail_path"] != `"/usr/sbin/sendmail -t -i"` {
		t.Errorf("previous value not captured: %v", a.State.Config.MailIniPrev)
	}
	if _, had := a.State.Config.MailIniPrev["8.3.9"]["sendmail_path"]; had {
		t.Error("8.3.9 had no active sendmail_path")
	}
	// Saved, so a later process can restore.
	st, _ := config.Load(a.Paths)
	if !st.Config.MailCatchPHP || len(st.Config.MailIniPrev) != 2 {
		t.Errorf("not persisted: %+v", st.Config)
	}

	// The SMTP port moves: the line follows, the capture stays original.
	mailpit.SMTPPort = 2525
	if ch := a.syncMailIni(); len(ch) != 2 {
		t.Errorf("changed = %v", ch)
	}
	if ini := readIni(t, a, "8.4.1"); !strings.Contains(ini, "127.0.0.1:2525") {
		t.Errorf("port not followed:\n%s", ini)
	}
	if ch := a.syncMailIni(); len(ch) != 0 {
		t.Errorf("an unchanged sync must not touch files: %v", ch)
	}

	if err := a.SetMailCatchPHP(false); err != nil {
		t.Fatal(err)
	}
	if got := readIni(t, a, "8.4.1"); got != strings.Replace(custom, `sendmail_path = "/usr/sbin/sendmail -t -i"`, `sendmail_path="/usr/sbin/sendmail -t -i"`, 1) {
		t.Errorf("8.4.1 not restored:\n%q", got)
	}
	if got := readIni(t, a, "8.3.9"); got != stock {
		t.Errorf("8.3.9 not restored:\n%q\nwant\n%q", got, stock)
	}
	if a.State.Config.MailIniPrev != nil || a.State.Config.MailCatchPHP {
		t.Errorf("state not cleared: %+v", a.State.Config)
	}
}

func TestCatchPHPMailNewVersionAndWindows(t *testing.T) {
	a := mailTestApp(t, map[string]string{"8.4.1": "SMTP = localhost\nsmtp_port = 25\n"})
	mailIniGOOS = "windows"
	if err := a.SetMailCatchPHP(true); err != nil {
		t.Fatal(err)
	}
	ini := readIni(t, a, "8.4.1")
	if !strings.Contains(ini, "SMTP=127.0.0.1\n") || !strings.Contains(ini, "smtp_port=1025") || strings.Contains(ini, "sendmail_path") {
		t.Errorf("windows ini:\n%s", ini)
	}
	// A version installed later is covered by the next sync (Apply).
	dir := a.Paths.PhpVersionDir("8.5.0")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "php.ini"), []byte("; fresh\n"), 0o644)
	if ch := a.syncMailIni(); len(ch) != 1 || ch[0] != "8.5.0" {
		t.Errorf("changed = %v", ch)
	}
	if err := a.SetMailCatchPHP(false); err != nil {
		t.Fatal(err)
	}
	if got := readIni(t, a, "8.4.1"); got != "SMTP=localhost\nsmtp_port=25\n" {
		t.Errorf("restored:\n%q", got)
	}
	if got := readIni(t, a, "8.5.0"); got != "; fresh\n" {
		t.Errorf("fresh restored:\n%q", got)
	}
}

func TestCatchPHPMailNeedsMail(t *testing.T) {
	a := mailTestApp(t, nil)
	a.State.Config.Mailpit = ""
	if err := a.SetMailCatchPHP(true); err == nil {
		t.Error("turning it on without Mail installed should fail")
	}
}

func TestMailCaddySiteAndHosts(t *testing.T) {
	a := mailTestApp(t, nil)
	mailpit.UIPort = 28025
	if err := a.WriteCaddyfile(nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.Paths.Caddyfile())
	cf := string(b)
	if !strings.Contains(cf, "\nmail.test {\n\ttls internal\n\treverse_proxy 127.0.0.1:28025 {\n\t\tflush_interval -1\n\t}\n") {
		t.Errorf("mail block missing:\n%s", cf)
	}
	if h := a.Hostnames(); len(h) != 1 || h[0] != "mail.test" {
		t.Errorf("hostnames %v", h)
	}
	// A linked site called "mail" wins; the inbox steps aside.
	a.State.Sites = append(a.State.Sites, config.Site{Name: "mail", Path: t.TempDir(), Kind: "static", BuildDir: "."})
	if a.mailSiteActive() {
		t.Error("the user's own mail site must win")
	}
	if err := a.WriteCaddyfile(nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(a.Paths.Caddyfile())
	if strings.Contains(string(b), "28025") {
		t.Error("no inbox proxy when a site is named mail")
	}
	// Not installed: nothing.
	a.State.Sites = nil
	a.State.Config.Mailpit = ""
	if h := a.Hostnames(); len(h) != 0 {
		t.Errorf("hostnames %v", h)
	}
}

func TestMailPorts(t *testing.T) {
	a, f := portsTestApp(t)
	oldSMTP, oldUI := mailpit.SMTPPort, mailpit.UIPort
	t.Cleanup(func() { mailpit.SMTPPort, mailpit.UIPort = oldSMTP, oldUI })
	ApplyPortConfig(config.Config{MailSMTPPort: 2525})
	if mailpit.SMTPPort != 2525 || mailpit.UIPort != 8025 {
		t.Fatalf("ports %d %d", mailpit.SMTPPort, mailpit.UIPort)
	}
	ApplyPortConfig(config.Config{})

	// Reserved even before Mail is installed.
	if err := a.checkPort(1025, "mysql", false, nil); err == nil || !strings.Contains(err.Error(), "Mail SMTP") {
		t.Errorf("1025 should be reserved for Mail: %v", err)
	}
	a.State.Config.Mailpit = "1.31.6"
	var kinds []string
	for _, p := range a.PortsOverview() {
		kinds = append(kinds, p.Kind)
	}
	if !strings.Contains(strings.Join(kinds, ","), "mail-smtp,mail-ui") {
		t.Errorf("kinds %v", kinds)
	}
	// Moving the inbox port while Mail is stopped: saved, nothing restarted.
	if _, err := a.SetEnginePort("mail-ui", 9025, false); err != nil {
		t.Fatal(err)
	}
	if mailpit.UIPort != 9025 || a.State.Config.MailUIPort != 9025 {
		t.Errorf("ui port %d cfg %d", mailpit.UIPort, a.State.Config.MailUIPort)
	}
	// Back to the default clears the override.
	if _, err := a.SetEnginePort("mailpit", 8025, false); err != nil {
		t.Fatal(err)
	}
	if a.State.Config.MailUIPort != 0 {
		t.Errorf("default should clear the override, got %d", a.State.Config.MailUIPort)
	}
	// A foreign process on the target port is reported.
	f.hold(2526, 4242, "MailHog")
	var inUse *PortInUseError
	if _, err := a.SetEnginePort("smtp", 2526, false); err == nil || !asPortInUse(err, &inUse) || inUse.Name != "MailHog" {
		t.Errorf("expected PortInUseError, got %v", err)
	}
}

func asPortInUse(err error, target **PortInUseError) bool {
	e, ok := err.(*PortInUseError)
	if ok {
		*target = e
	}
	return ok
}

func TestMailEnvSnippet(t *testing.T) {
	s := MailEnvSnippet(2525)
	for _, want := range []string{"MAIL_MAILER=smtp\n", "MAIL_HOST=127.0.0.1\n", "MAIL_PORT=2525\n", "MAIL_USERNAME=null\n",
		"MAIL_PASSWORD=null\n", "MAIL_SCHEME=null\n", "MAIL_ENCRYPTION=null\n", `MAIL_FROM_ADDRESS="hello@example.test"` + "\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("snippet lacks %q", want)
		}
	}
	if !strings.Contains(MailNodeSnippet(2525), "port: 2525,") {
		t.Error("node snippet port")
	}
}

func TestUninstallMailRestoresIni(t *testing.T) {
	a := mailTestApp(t, map[string]string{"8.4.1": "memory_limit=1G\n"})
	if err := a.SetMailCatchPHP(true); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(mailpit.BaseDir(a.Paths), 0o755)
	os.WriteFile(mailpit.DBFile(a.Paths), []byte("db"), 0o644)
	if err := a.UninstallMail(true); err != nil {
		t.Fatal(err)
	}
	if got := readIni(t, a, "8.4.1"); got != "memory_limit=1G\n" {
		t.Errorf("ini not restored: %q", got)
	}
	if a.State.Config.Mailpit != "" || a.State.Config.MailCatchPHP {
		t.Errorf("config %+v", a.State.Config)
	}
	if _, err := os.Stat(mailpit.DBFile(a.Paths)); err != nil {
		t.Error("--keep-mail must keep the mailbox")
	}
}
