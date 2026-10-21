package mailpit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pm/internal/pmdir"
)

// TestE2E is a manual end-to-end check against a REAL Mailpit binary in
// a scratch home — skipped unless MAILPIT_E2E_BIN points at one (e.g. a
// release archive unpacked into a scratch folder). It runs on
// non-default ports (21025/28025, plus a second Mailpit on 21026/28026
// standing in for a "real" SMTP server to release to), so it never
// touches a Mailpit the developer runs, and never sends mail anywhere
// but those two local instances.
func TestE2E(t *testing.T) {
	src := os.Getenv("MAILPIT_E2E_BIN")
	if src == "" {
		t.Skip("set MAILPIT_E2E_BIN to a Mailpit binary to run")
	}
	home := t.TempDir()
	paths := pmdir.Paths{Home: home}
	if err := paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	ver := "9.9.9"
	exe := Exe(paths, ver)
	os.MkdirAll(filepath.Dir(exe), 0o755)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatal(err)
	}

	oldS, oldU := SMTPPort, UIPort
	SMTPPort, UIPort = 21025, 28025
	t.Cleanup(func() { SMTPPort, UIPort = oldS, oldU })
	if Running() || SMTPUp() {
		t.Fatal("ports 21025/28025 are busy")
	}

	// The stand-in "real" server for releases.
	// Run from outside Mullion's folder, as an unrelated server would.
	targetExe := filepath.Join(home, "target-mailpit")
	if err := os.WriteFile(targetExe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	target := exec.Command(targetExe, "--smtp", "127.0.0.1:21026", "--listen", "127.0.0.1:28026",
		"--database", filepath.Join(home, "target.db"), "--smtp-auth-accept-any", "--smtp-auth-allow-insecure", "--disable-version-check")
	if err := target.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Process.Kill(); target.Wait() })
	for i := 0; i < 50 && !portUp(21026); i++ {
		time.Sleep(100 * time.Millisecond)
	}

	if err := SaveRelay(paths, RelayConfig{Enabled: true, Host: "127.0.0.1", Port: 21026, Security: "none",
		Username: "u", Password: "p", AllowedRecipients: `@example\.test$`}); err != nil {
		t.Fatal(err)
	}
	if err := Start(paths, ver); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Stop(paths) })
	if !Owned(paths) {
		t.Fatal("Owned() should see the server")
	}
	if err := Start(paths, ver); err != nil { // idempotent
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := SendTest(TestMessage{To: []string{"Bob <bob@example.test>"}, Subject: "E2E ✓", Text: "plain", HTML: `<p>html <a href="http://127.0.0.1:1/x">x</a></p>`,
		Attachments: []Attachment{{Name: "a.txt", Data: []byte("hello")}}}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		cmd := exec.Command("/bin/sh", "-c", SendmailCommand(exe, SMTPPort)+" -t -i")
		cmd.Stdin = strings.NewReader("From: php@example.test\r\nTo: amy@example.test\r\nSubject: via sendmail\r\n\r\nbody\r\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sendmail: %v %s", err, out)
		}
	}
	var l List
	for i := 0; i < 20; i++ {
		if l, err = Messages(ctx, 50); err == nil && l.Total >= 2 || runtime.GOOS == "windows" && l.Total >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("inbox: total=%d unread=%d", l.Total, l.Unread)
	var id string
	for _, m := range l.Messages {
		t.Logf("  %s from=%v to=%v subject=%q attachments=%d", m.ID, m.From, m.To, m.Subject, m.Attachments)
		if m.Subject == "E2E ✓" {
			id = m.ID
		}
	}
	if id == "" {
		t.Fatal("test message not in the inbox")
	}
	m, err := GetMessage(ctx, id)
	if err != nil || !strings.Contains(m.HTML, "html") || m.Text != "plain" || len(m.Attachments) != 1 || m.Attachments[0].FileName != "a.txt" {
		t.Fatalf("message %+v %v", m, err)
	}
	if h, err := Headers(ctx, id); err != nil || h["X-Mailer"][0] != "Mullion" {
		t.Errorf("headers %v %v", h, err)
	}
	if raw, err := Raw(ctx, id); err != nil || !strings.Contains(raw, "multipart/mixed") {
		t.Errorf("raw %v", err)
	}
	if hc, err := CheckHTML(ctx, id); err != nil || hc.Total.Tests == 0 {
		t.Errorf("html check %+v %v", hc.Total, err)
	} else {
		t.Logf("html check: %+v", hc.Total)
	}
	if lc, err := CheckLinks(ctx, id); err != nil || len(lc.Links) != 1 {
		t.Errorf("link check %+v %v", lc, err)
	} else {
		t.Logf("link check: errors=%d %+v", lc.Errors, lc.Links)
	}
	if w, err := UIConfig(ctx); err != nil || !w.MessageRelay.Enabled || w.MessageRelay.SMTPServer != "127.0.0.1:21026" {
		t.Errorf("relay not active: %+v %v", w, err)
	}
	if err := ReleaseMessage(ctx, id, []string{"someone@gmail.com"}); err == nil {
		t.Error("the allowed-recipients pattern should refuse gmail.com")
	} else {
		t.Logf("refused release: %v", err)
	}
	if err := ReleaseMessage(ctx, id, []string{"me@example.test"}); err != nil {
		t.Fatal(err)
	}
	// It arrived at the stand-in server.
	oldBase := baseURL
	baseURL = func() string { return "http://127.0.0.1:28026" }
	tl, err := Messages(ctx, 10)
	baseURL = oldBase
	if err != nil || tl.Total != 1 || tl.Messages[0].Subject != "E2E ✓" {
		t.Errorf("released message not delivered: %+v %v", tl, err)
	}

	if err := DeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	if l, _ := Messages(ctx, 50); l.Total != 0 {
		t.Errorf("delete all left %d", l.Total)
	}
	if err := Stop(paths); err != nil {
		t.Fatal(err)
	}
	if Running() || SMTPUp() || Owned(paths) {
		t.Error("still running after Stop")
	}
	if b, _ := os.ReadFile(LogFile(paths)); len(b) > 0 {
		t.Logf("mailpit.log:\n%s", b)
	}
}
