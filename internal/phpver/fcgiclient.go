package phpver

// A minimal FastCGI client (the responder role, one request per
// connection) — enough to run one PHP script through a version's
// php-fpm/php-cgi the way Caddy does, so what it reports is what sites
// see. The standard library only has the server side (net/http/fcgi).

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	fcgiVersion      = 1
	fcgiBeginRequest = 1
	fcgiEndRequest   = 3
	fcgiParams       = 4
	fcgiStdin        = 5
	fcgiStdout       = 6
	fcgiStderr       = 7
	fcgiResponder    = 1
	fcgiRequestID    = 1
)

func fcgiRecord(w io.Writer, typ byte, content []byte) error {
	for {
		chunk := content
		if len(chunk) > 65535 {
			chunk = chunk[:65535]
		}
		pad := (8 - len(chunk)%8) % 8
		h := []byte{fcgiVersion, typ, 0, fcgiRequestID, byte(len(chunk) >> 8), byte(len(chunk)), byte(pad), 0}
		if _, err := w.Write(append(append(h, chunk...), make([]byte, pad)...)); err != nil {
			return err
		}
		content = content[len(chunk):]
		if len(content) == 0 {
			return nil
		}
	}
}

// fcgiPairLen encodes a name-value length (1 byte, or 4 with the high
// bit set).
func fcgiPairLen(b *bytes.Buffer, n int) {
	if n < 128 {
		b.WriteByte(byte(n))
		return
	}
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], uint32(n)|1<<31)
	b.Write(x[:])
}

func fcgiPairs(params map[string]string) []byte {
	var b bytes.Buffer
	for k, v := range params {
		fcgiPairLen(&b, len(k))
		fcgiPairLen(&b, len(v))
		b.WriteString(k)
		b.WriteString(v)
	}
	return b.Bytes()
}

// fcgiGet runs script with a GET request and returns the response body
// (an error for a non-2xx Status header or a PHP error on stderr with an
// empty body).
func fcgiGet(addr, script string, timeout time.Duration) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("PHP isn't answering on %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	w := bufio.NewWriter(conn)
	begin := []byte{0, fcgiResponder, 0, 0, 0, 0, 0, 0}
	if err := fcgiRecord(w, fcgiBeginRequest, begin); err != nil {
		return nil, err
	}
	params := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_SOFTWARE":   "mullion",
		"SERVER_PROTOCOL":   "HTTP/1.1",
		"SERVER_NAME":       "localhost",
		"SERVER_ADDR":       "127.0.0.1",
		"SERVER_PORT":       "80",
		"REMOTE_ADDR":       "127.0.0.1",
		"REQUEST_METHOD":    "GET",
		"REQUEST_URI":       "/" + filepath.Base(script),
		"SCRIPT_NAME":       "/" + filepath.Base(script),
		"SCRIPT_FILENAME":   script,
		"DOCUMENT_ROOT":     filepath.Dir(script),
		"QUERY_STRING":      "",
		"CONTENT_LENGTH":    "0",
	}
	if err := fcgiRecord(w, fcgiParams, fcgiPairs(params)); err != nil {
		return nil, err
	}
	if err := fcgiRecord(w, fcgiParams, nil); err != nil {
		return nil, err
	}
	if err := fcgiRecord(w, fcgiStdin, nil); err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn)
	var stdout, stderr bytes.Buffer
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("reading PHP's answer: %w", err)
		}
		n := int(h[4])<<8 | int(h[5])
		body := make([]byte, n+int(h[6]))
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("reading PHP's answer: %w", err)
		}
		body = body[:n]
		switch h[1] {
		case fcgiStdout:
			stdout.Write(body)
		case fcgiStderr:
			stderr.Write(body)
		case fcgiEndRequest:
			return fcgiBody(stdout.Bytes(), stderr.String())
		}
	}
}

// fcgiBody splits a CGI response into headers and body.
func fcgiBody(out []byte, stderr string) ([]byte, error) {
	sep := bytes.Index(out, []byte("\r\n\r\n"))
	n := 4
	if sep < 0 {
		sep, n = bytes.Index(out, []byte("\n\n")), 2
	}
	if sep < 0 {
		if msg := strings.TrimSpace(stderr); msg != "" {
			return nil, errors.New("PHP: " + msg)
		}
		return nil, fmt.Errorf("PHP sent no CGI headers")
	}
	hdr, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(out[:sep], "\r\n\r\n"...)))).ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("PHP sent bad CGI headers: %w", err)
	}
	body := out[sep+n:]
	if st := hdr.Get("Status"); st != "" {
		code, _ := strconv.Atoi(strings.Fields(st)[0])
		if code >= 300 {
			msg := strings.TrimSpace(stderr)
			if msg == "" {
				msg = strings.TrimSpace(string(body))
			}
			return nil, fmt.Errorf("PHP answered %s: %s", st, msg)
		}
	}
	return body, nil
}
