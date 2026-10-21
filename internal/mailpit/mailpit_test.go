package mailpit

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pm/internal/pmdir"
)

func TestAssetName(t *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "arm64"}:  "mailpit-darwin-arm64.tar.gz",
		{"darwin", "amd64"}:  "mailpit-darwin-amd64.tar.gz",
		{"windows", "amd64"}: "mailpit-windows-amd64.zip",
		{"windows", "arm64"}: "mailpit-windows-arm64.zip",
		{"windows", "386"}:   "",
		{"plan9", "amd64"}:   "",
	}
	for in, want := range cases {
		if got := assetName(in[0], in[1]); got != want {
			t.Errorf("assetName(%s/%s) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// The asset list of the real v1.31.6 release.
var realAssets = []Asset{
	{Name: "mailpit-darwin-amd64.tar.gz"}, {Name: "mailpit-darwin-arm64.tar.gz"},
	{Name: "mailpit-linux-386.tar.gz"}, {Name: "mailpit-linux-amd64.tar.gz"},
	{Name: "mailpit-linux-arm.tar.gz"}, {Name: "mailpit-linux-arm64.tar.gz"},
	{Name: "mailpit-windows-amd64.zip"}, {Name: "mailpit-windows-arm64.zip"},
}

func TestPickAsset(t *testing.T) {
	a, err := pickAsset(realAssets, "darwin", "arm64")
	if err != nil || a.Name != "mailpit-darwin-arm64.tar.gz" {
		t.Errorf("darwin/arm64: %v %v", a, err)
	}
	a, err = pickAsset(realAssets, "windows", "amd64")
	if err != nil || a.Name != "mailpit-windows-amd64.zip" {
		t.Errorf("windows/amd64: %v %v", a, err)
	}
	if _, err := pickAsset(realAssets[:1], "darwin", "arm64"); err == nil {
		t.Error("expected an error when the archive is missing")
	}
	if _, err := pickAsset(realAssets, "windows", "386"); err == nil {
		t.Error("expected an error for an unpublished platform")
	}
}

func TestArgs(t *testing.T) {
	old1, old2 := SMTPPort, UIPort
	t.Cleanup(func() { SMTPPort, UIPort = old1, old2 })
	SMTPPort, UIPort = 2525, 9025
	paths := pmdir.Paths{Home: t.TempDir()}
	want := []string{
		"--smtp", "127.0.0.1:2525",
		"--listen", "127.0.0.1:9025",
		"--database", filepath.Join(paths.Home, "mailpit", "mailpit.db"),
		"--smtp-auth-accept-any",
		"--smtp-auth-allow-insecure",
		"--max", "1000",
		"--disable-version-check",
	}
	if got := Args(paths); !slices.Equal(got, want) {
		t.Errorf("Args =\n%q\nwant\n%q", got, want)
	}
	// An enabled relay adds its config file; a disabled one doesn't.
	if err := SaveRelay(paths, RelayConfig{Enabled: true, Host: "smtp.example.com", Port: 587, Security: "starttls"}); err != nil {
		t.Fatal(err)
	}
	got := Args(paths)
	if !slices.Equal(got[len(got)-2:], []string{"--smtp-relay-config", RelayYAML(paths)}) {
		t.Errorf("relay not passed: %q", got)
	}
	if err := SaveRelay(paths, RelayConfig{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if got := Args(paths); slices.Contains(got, "--smtp-relay-config") {
		t.Errorf("disabled relay still passed: %q", got)
	}
}

func TestSendmailCommand(t *testing.T) {
	if got := SendmailCommand("/Users/me/.mullion/mailpit/1.31.6/mailpit", 1025); got != "/Users/me/.mullion/mailpit/1.31.6/mailpit sendmail -S 127.0.0.1:1025" {
		t.Errorf("got %q", got)
	}
	if got := SendmailCommand("/Users/Jo Doe/.mullion/mailpit/1.0.0/mailpit", 2525); got != "'/Users/Jo Doe/.mullion/mailpit/1.0.0/mailpit' sendmail -S 127.0.0.1:2525" {
		t.Errorf("spaces: got %q", got)
	}
}

func TestNewer(t *testing.T) {
	if !Newer("1.31.10", "1.31.9") || Newer("1.31.6", "1.31.6") || Newer("1.2.0", "1.10.0") {
		t.Error("Newer compares numerically")
	}
}

// fakeTarGz builds a release archive like Mailpit's.
func fakeTarGz(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"mailpit": "#!/bin/sh\necho fake\n", "LICENSE": "MIT", "README.md": "readme"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestInstallRelease(t *testing.T) {
	archive := fakeTarGz(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			json.NewEncoder(w).Encode(Release{Tag: "v1.31.6", Assets: []Asset{
				{Name: "mailpit-darwin-arm64.tar.gz", URL: "http://" + r.Host + "/dl/mailpit-darwin-arm64.tar.gz"},
			}})
		case "/dl/mailpit-darwin-arm64.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = old })

	paths := pmdir.Paths{Home: t.TempDir()}
	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sawProgress := false
	v, err := installRelease(context.Background(), paths, rel, "darwin", "arm64", func(done, total int64) { sawProgress = done > 0 })
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.31.6" || !sawProgress {
		t.Errorf("version %q progress %v", v, sawProgress)
	}
	bin := filepath.Join(paths.Home, "mailpit", "1.31.6", "mailpit")
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Errorf("binary not executable: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "mailpit", "1.31.6", "LICENSE")); err != nil {
		t.Error("LICENSE should be kept next to the binary")
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "mailpit", "1.31.6", "README.md")); err == nil {
		t.Error("README should not be installed")
	}
	if runtime.GOOS != "windows" {
		if got := Installed(paths); !slices.Equal(got, []string{"1.31.6"}) {
			t.Errorf("Installed = %v", got)
		}
	}
	// No staging leftovers.
	entries, _ := os.ReadDir(filepath.Join(paths.Home, "mailpit"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("staging dir left behind: %s", e.Name())
		}
	}
}

func TestRemoveKeepMail(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	base := filepath.Join(paths.Home, "mailpit")
	for _, f := range []string{"1.31.6/mailpit", "mailpit.db", "mailpit.db-wal", "relay.json"} {
		os.MkdirAll(filepath.Dir(filepath.Join(base, f)), 0o755)
		os.WriteFile(filepath.Join(base, f), []byte("x"), 0o644)
	}
	if err := Remove(paths, true); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{"1.31.6": false, "mailpit.db": true, "mailpit.db-wal": true, "relay.json": false} {
		_, err := os.Stat(filepath.Join(base, f))
		if (err == nil) != want {
			t.Errorf("%s present=%v, want %v", f, err == nil, want)
		}
	}
	if err := Remove(paths, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); err == nil {
		t.Error("Remove(keepMail=false) should delete everything")
	}
}

func TestRelayYAMLAndPermissions(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	c := RelayConfig{Enabled: true, Host: "smtp.gmail.com", Port: 587, Username: "me@gmail.com", Password: `p"a\ss`,
		Security: "starttls", AllowedRecipients: `@mycompany\.com$`}
	if err := SaveRelay(paths, c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(RelayYAML(paths))
	if err != nil {
		t.Fatal(err)
	}
	y := string(b)
	for _, want := range []string{
		`host: "smtp.gmail.com"`, "port: 587", "starttls: true", "tls: false", "auth: plain",
		`username: "me@gmail.com"`, `password: "p\"a\\ss"`, `allowed-recipients: "@mycompany\\.com$"`,
	} {
		if !strings.Contains(y, want+"\n") {
			t.Errorf("relay.yaml lacks %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "allow-insecure") {
		t.Error("allow-insecure is only for unencrypted logins")
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{RelayYAML(paths), relayJSON(paths)} {
			if info, _ := os.Stat(f); info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v, want 0600", f, info.Mode().Perm())
			}
		}
	}
	// An empty password on save keeps the stored one.
	c.Password = ""
	c.Port = 2525
	if err := SaveRelay(paths, c); err != nil {
		t.Fatal(err)
	}
	if got := LoadRelay(paths); got.Password != `p"a\ss` || got.Port != 2525 {
		t.Errorf("reload = %+v", got)
	}
	// Validation.
	for _, bad := range []RelayConfig{
		{Enabled: true, Host: "bad host", Port: 25, Security: "none"},
		{Enabled: true, Host: "h", Port: 0, Security: "none"},
		{Enabled: true, Host: "h", Port: 25, Security: "ssl"},
		{Enabled: true, Host: "h", Port: 25, Security: "none", AllowedRecipients: "("},
	} {
		if bad.Validate() == nil {
			t.Errorf("expected %+v to be rejected", bad)
		}
	}
	// No auth, no TLS: no credentials in the file.
	y = RenderRelayYAML(RelayConfig{Host: "127.0.0.1", Port: 2525, Security: "none"})
	if !strings.Contains(y, "auth: none\n") || strings.Contains(y, "username") {
		t.Errorf("unauthenticated relay:\n%s", y)
	}
}

func TestBuildMessage(t *testing.T) {
	data, from, to, err := BuildMessage(TestMessage{
		To:          []string{"Bob <bob@example.test>, amy@example.test"},
		Subject:     "Héllo",
		Text:        "plain body",
		HTML:        "<p>html body</p>",
		Attachments: []Attachment{{Name: "../../notes.txt", Data: []byte("attached!")}},
	}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if from != DefaultFrom || !slices.Equal(to, []string{"bob@example.test", "amy@example.test"}) {
		t.Errorf("envelope %q %q", from, to)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	if s, _ := dec.DecodeHeader(msg.Header.Get("Subject")); s != "Héllo" {
		t.Errorf("subject %q", s)
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("top type %q", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	alt, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	amt, aparams, _ := mime.ParseMediaType(alt.Header.Get("Content-Type"))
	if amt != "multipart/alternative" {
		t.Fatalf("first part %q", amt)
	}
	ar := multipart.NewReader(alt, aparams["boundary"])
	var types, bodies []string
	for {
		p, err := ar.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		types, bodies = append(types, ct), append(bodies, string(b))
	}
	if !slices.Equal(types, []string{"text/plain", "text/html"}) || bodies[0] != "plain body" || bodies[1] != "<p>html body</p>" {
		t.Errorf("alternative parts %q %q", types, bodies)
	}
	att, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if ct := att.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "notes.txt") {
		t.Errorf("attachment type %q", ct)
	}
	if att.FileName() != "notes.txt" {
		t.Errorf("attachment name %q (path must be stripped)", att.FileName())
	}
	raw, _ := io.ReadAll(att)
	if got, _ := decodeB64(string(raw)); got != "attached!" {
		t.Errorf("attachment body %q", got)
	}

	// Validation.
	if _, _, _, err := BuildMessage(TestMessage{}, time.Now()); err == nil {
		t.Error("no recipient should fail")
	}
	if _, _, _, err := BuildMessage(TestMessage{To: []string{"not an address"}}, time.Now()); err == nil {
		t.Error("a bad recipient should fail")
	}
	if _, _, _, err := BuildMessage(TestMessage{To: []string{"a@b.test"}, Subject: "x\r\nBcc: evil@x"}, time.Now()); err == nil {
		t.Error("header injection through the subject should fail")
	}
	// Text only: a single part.
	data, _, _, _ = BuildMessage(TestMessage{To: []string{"a@b.test"}, Text: "hi"}, time.Now())
	msg, _ = mail.ReadMessage(bytes.NewReader(data))
	if ct := msg.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("text-only type %q", ct)
	}
}

func decodeB64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(s, "\r\n", ""))
	return string(b), err
}

// fakeSMTP is a minimal SMTP server that records one transaction.
type fakeSMTP struct {
	mu   sync.Mutex
	from string
	rcpt []string
	data string
}

func startFakeSMTP(t *testing.T) (*fakeSMTP, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f, ln.Addr().String()
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { c.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			say("250 fake")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.from = strings.Trim(cmd[10:], "<> ")
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, strings.Trim(cmd[8:], "<> "))
			f.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestSendTest(t *testing.T) {
	f, addr := startFakeSMTP(t)
	old := smtpAddr
	smtpAddr = func() string { return addr }
	t.Cleanup(func() { smtpAddr = old })
	err := SendTest(TestMessage{From: "app@example.test", To: []string{"me@example.test"}, Subject: "Hi", HTML: "<b>x</b>"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.from != "app@example.test" || !slices.Equal(f.rcpt, []string{"me@example.test"}) {
		t.Errorf("envelope %q %q", f.from, f.rcpt)
	}
	if !strings.Contains(f.data, "Subject: Hi\r\n") || !strings.Contains(f.data, "Content-Type: text/html; charset=utf-8") {
		t.Errorf("data:\n%s", f.data)
	}
}

// withAPI points the REST client at a fake Mailpit.
func withAPI(t *testing.T, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = func() string { return srv.URL }
	t.Cleanup(func() { baseURL = old })
}

func TestAPIClient(t *testing.T) {
	var gotRelease, gotDelete string
	withAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/messages":
			if r.URL.Query().Get("limit") != "50" {
				t.Errorf("limit %q", r.URL.RawQuery)
			}
			// Trimmed from a real v1.31.6 answer.
			w.Write([]byte(`{"total":2,"unread":1,"count":2,"messages_count":2,"messages_unread":1,"start":0,"tags":[],"messages":[
			 {"ID":"a1","MessageID":"x@mailpit","Read":false,"From":{"Name":"App","Address":"app@example.test"},"To":[{"Name":"Bob","Address":"bob@example.test"}],"Cc":null,"Bcc":null,"ReplyTo":[],"Subject":"Hello","Created":"2026-10-21T13:23:50.537+03:00","Username":"","Tags":[],"Size":413,"Attachments":0,"Snippet":"Hi there"},
			 {"ID":"b2","Read":true,"From":null,"To":[],"Subject":"","Created":"2026-10-21T13:20:00+03:00","Size":1,"Attachments":2,"Snippet":""}]}`))
		case r.Method == "POST" && r.URL.Path == "/api/v1/message/a1/release":
			b, _ := io.ReadAll(r.Body)
			gotRelease = string(b)
			w.Write([]byte("ok"))
		case r.Method == "POST" && r.URL.Path == "/api/v1/message/bad/release":
			http.Error(w, "Failed: 1 Address do not match the allowlist: x@gmail.com", http.StatusBadRequest)
		case r.Method == "DELETE" && r.URL.Path == "/api/v1/messages":
			b, _ := io.ReadAll(r.Body)
			gotDelete = string(b)
			w.Write([]byte("ok"))
		case r.URL.Path == "/api/v1/message/a1/raw":
			w.Write([]byte("Subject: Hello\r\n\r\nbody"))
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	l, err := Messages(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if l.Total != 2 || l.Unread != 1 || len(l.Messages) != 2 {
		t.Fatalf("list %+v", l)
	}
	m := l.Messages[0]
	if m.ID != "a1" || m.Read || m.From.Address != "app@example.test" || m.To[0].Name != "Bob" || m.Subject != "Hello" || m.Snippet != "Hi there" {
		t.Errorf("summary %+v", m)
	}
	if l.Messages[1].From != nil || l.Messages[1].Attachments != 2 {
		t.Errorf("second %+v", l.Messages[1])
	}
	if err := ReleaseMessage(ctx, "a1", []string{"me@example.test"}); err != nil {
		t.Fatal(err)
	}
	if gotRelease != `{"To":["me@example.test"]}` {
		t.Errorf("release body %s", gotRelease)
	}
	if err := ReleaseMessage(ctx, "bad", []string{"x@gmail.com"}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("release error should carry Mailpit's text, got %v", err)
	}
	if err := DeleteAll(ctx); err != nil || gotDelete != `{}` {
		t.Errorf("delete all: %v %q", err, gotDelete)
	}
	if err := Delete(ctx, []string{"a1"}); err != nil || gotDelete != `{"IDs":["a1"]}` {
		t.Errorf("delete: %v %q", err, gotDelete)
	}
	if raw, err := Raw(ctx, "a1"); err != nil || !strings.HasPrefix(raw, "Subject: Hello") {
		t.Errorf("raw %q %v", raw, err)
	}
}
