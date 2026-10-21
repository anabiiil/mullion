package mailpit

// Sending a test message into the inbox — over real SMTP to Mailpit's
// port, exactly the path an app's mailer takes.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Attachment is a file attached to a test message.
type Attachment struct {
	Name        string
	ContentType string // "" = guessed from the name
	Data        []byte
}

// TestMessage is what "Send test email" composes. Text and HTML may both
// be set (multipart/alternative); at least one of them should be.
type TestMessage struct {
	From        string
	To          []string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
}

// MaxAttachment bounds one attachment of a test message.
const MaxAttachment = 10 << 20

// DefaultFrom is the sender a test message gets when none is given.
const DefaultFrom = "hello@example.test"

// parseAddrs validates a list of addresses, accepting "a@x, b@y" in one
// entry too.
func parseAddrs(in []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, s := range in {
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			a, err := mail.ParseAddress(part)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q", part)
			}
			out = append(out, a)
		}
	}
	return out, nil
}

// BuildMessage renders a TestMessage as an RFC 5322 message: plain text,
// HTML, or both (multipart/alternative), wrapped in multipart/mixed when
// there are attachments. It returns the envelope sender and recipients
// with it.
func BuildMessage(m TestMessage, now time.Time) (data []byte, from string, to []string, err error) {
	if strings.TrimSpace(m.From) == "" {
		m.From = DefaultFrom
	}
	fromAddrs, err := parseAddrs([]string{m.From})
	if err != nil {
		return nil, "", nil, err
	}
	if len(fromAddrs) != 1 {
		return nil, "", nil, errors.New("give exactly one From address")
	}
	toAddrs, err := parseAddrs(m.To)
	if err != nil {
		return nil, "", nil, err
	}
	if len(toAddrs) == 0 {
		return nil, "", nil, errors.New("give at least one To address")
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, "", nil, errors.New("the subject must be a single line")
	}
	for _, a := range m.Attachments {
		if len(a.Data) > MaxAttachment {
			return nil, "", nil, fmt.Errorf("%s is larger than %d MB", a.Name, MaxAttachment>>20)
		}
	}
	if m.Text == "" && m.HTML == "" {
		m.Text = "This is a test message sent by Mullion."
	}

	var b bytes.Buffer
	tos := make([]string, len(toAddrs))
	for i, a := range toAddrs {
		tos[i] = a.String()
		to = append(to, a.Address)
	}
	fmt.Fprintf(&b, "From: %s\r\n", fromAddrs[0].String())
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(tos, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@mullion.local>\r\n", randomID())
	b.WriteString("MIME-Version: 1.0\r\nX-Mailer: Mullion\r\n")

	body, bodyType, err := buildBody(m)
	if err != nil {
		return nil, "", nil, err
	}
	if len(m.Attachments) == 0 {
		b.WriteString(bodyType)
		b.Write(body)
		return b.Bytes(), fromAddrs[0].Address, to, nil
	}
	mw := multipart.NewWriter(&b)
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", mw.Boundary())
	pw, err := mw.CreatePart(headerFromBlock(bodyType))
	if err != nil {
		return nil, "", nil, err
	}
	pw.Write(body)
	for _, a := range m.Attachments {
		name := filepath.Base(strings.ReplaceAll(a.Name, `\`, "/"))
		if name == "" || name == "." || name == "/" {
			name = "attachment"
		}
		ct := a.ContentType
		if ct == "" {
			ct = mime.TypeByExtension(filepath.Ext(name))
		}
		// TypeByExtension (and browsers) may add parameters
		// ("text/plain; charset=utf-8"); keep them next to the name.
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil || mt == "" {
			mt, params = "application/octet-stream", map[string]string{}
		}
		params["name"] = name
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", mime.FormatMediaType(mt, params))
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		h.Set("Content-Transfer-Encoding", "base64")
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", nil, err
		}
		writeBase64Lines(w, a.Data)
	}
	if err := mw.Close(); err != nil {
		return nil, "", nil, err
	}
	return b.Bytes(), fromAddrs[0].Address, to, nil
}

// buildBody renders the text/HTML part(s): the returned type is the
// header block (ending in a blank line) that introduces the body.
func buildBody(m TestMessage) ([]byte, string, error) {
	part := func(ct, content string) ([]byte, string) {
		var buf bytes.Buffer
		qp := quotedprintable.NewWriter(&buf)
		qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\n", "\r\n")))
		qp.Close()
		return buf.Bytes(), "Content-Type: " + ct + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"
	}
	switch {
	case m.HTML == "":
		b, h := part("text/plain", m.Text)
		return b, h, nil
	case m.Text == "":
		b, h := part("text/html", m.HTML)
		return b, h, nil
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range []struct{ ct, content string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b, h := part(p.ct, p.content)
		w, err := mw.CreatePart(headerFromBlock(h))
		if err != nil {
			return nil, "", err
		}
		w.Write(b)
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q\r\n\r\n", mw.Boundary()), nil
}

// headerFromBlock parses a "Key: value\r\n...\r\n\r\n" block.
func headerFromBlock(block string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	for _, line := range strings.Split(strings.TrimSpace(block), "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return h
}

func writeBase64Lines(w interface{ Write([]byte) (int, error) }, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		w.Write([]byte(enc[:76] + "\r\n"))
		enc = enc[76:]
	}
	if enc != "" {
		w.Write([]byte(enc + "\r\n"))
	}
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// sendMail is net/smtp.SendMail — a seam for tests.
var sendMail = smtp.SendMail

// smtpAddr is where test messages go; a variable so tests can point it
// at a fake SMTP server.
var smtpAddr = func() string { return "127.0.0.1:" + strconv.Itoa(SMTPPort) }

// SendTest delivers a test message to Mailpit over SMTP (no login: it
// accepts any).
func SendTest(m TestMessage) error {
	data, from, to, err := BuildMessage(m, time.Now())
	if err != nil {
		return err
	}
	if err := sendMail(smtpAddr(), nil, from, to, data); err != nil {
		if !SMTPUp() {
			return ErrNotRunning
		}
		return fmt.Errorf("sending the test message: %w", err)
	}
	return nil
}
