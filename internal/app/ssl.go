package app

import (
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"pm/internal/caddy"
	"pm/internal/proc"
)

// SSLInfo describes Caddy's local certificate authority — the root that
// signs every https://*.<tld> certificate — for the SSL page and
// `mullion ssl`.
type SSLInfo struct {
	CAPath   string `json:"caPath"`
	CAExists bool   `json:"caExists"`
	// Trusted: the OS trust store accepts the root (so browsers other
	// than Firefox show the padlock).
	Trusted     bool   `json:"trusted"`
	CASubject   string `json:"caSubject,omitempty"`
	CAExpires   string `json:"caExpires,omitempty"` // YYYY-MM-DD
	SecureSites int    `json:"secureSites"`
	TotalSites  int    `json:"totalSites"`
}

// caCert is what SSLInfo needs from the root certificate.
type caCert struct {
	subject    string
	expires    time.Time
	thumbprint string // uppercase SHA-1 of the DER, as Windows shows it
}

// parseCA reads a PEM root certificate.
func parseCA(data []byte) (caCert, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return caCert{}, errors.New("not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return caCert{}, err
	}
	sum := sha1.Sum(cert.Raw)
	subject := cert.Subject.CommonName
	if subject == "" {
		subject = cert.Subject.String()
	}
	return caCert{
		subject:    subject,
		expires:    cert.NotAfter,
		thumbprint: strings.ToUpper(hex.EncodeToString(sum[:])),
	}, nil
}

// caTrusted asks the OS (read-only) whether the root at path is trusted.
// Swapped by tests.
var caTrusted = func(path, thumbprint string) bool {
	switch runtime.GOOS {
	case "darwin":
		// Evaluates the root against the user's and the system's trust
		// settings (where `caddy trust` records it); -L stays offline.
		return exec.Command("security", "verify-cert", "-c", path, "-L", "-q").Run() == nil
	case "windows":
		// `caddy trust` adds the root to the ROOT store; the current
		// user's view of it includes the machine's.
		script := fmt.Sprintf(`if ((Test-Path 'Cert:\CurrentUser\Root\%[1]s') -or (Test-Path 'Cert:\LocalMachine\Root\%[1]s')) { 'yes' }`, thumbprint)
		out, err := proc.Quiet("powershell", "-NoProfile", "-Command", script).Output()
		return err == nil && strings.TrimSpace(string(out)) == "yes"
	}
	return false
}

// SSLInfo reports on the local CA and how many sites use HTTPS.
// Read-only: it parses the root certificate and queries the trust store.
func (a *App) SSLInfo() SSLInfo {
	info := SSLInfo{CAPath: caddy.RootCertPath(), TotalSites: len(a.State.Sites)}
	for _, s := range a.State.Sites {
		if s.Secure {
			info.SecureSites++
		}
	}
	data, err := os.ReadFile(info.CAPath)
	if err != nil {
		return info
	}
	info.CAExists = true
	ca, err := parseCA(data)
	if err != nil {
		return info
	}
	info.CASubject = ca.subject
	info.CAExpires = ca.expires.Format("2006-01-02")
	info.Trusted = caTrusted(info.CAPath, ca.thumbprint)
	return info
}

// TrustCA installs Caddy's root into the OS trust store (`caddy
// trust`): one password prompt on macOS, one UAC prompt on Windows.
func (a *App) TrustCA() error {
	return caddy.TrustCA(a.Paths)
}

// SecureAll switches every site to HTTPS (secure=true) or back to plain
// HTTP, then converges once.
func (a *App) SecureAll(secure bool) error {
	for i := range a.State.Sites {
		a.State.Sites[i].Secure = secure
	}
	return a.applyIfEnabled()
}

// ExportCA copies the root certificate to dest — for browsers and
// devices that keep their own trust store (Firefox, phones, VMs). A
// directory dest gets ExportCAName inside it (see ExportCATarget).
func (a *App) ExportCA(dest string) error {
	src := caddy.RootCertPath()
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no local CA yet — secure a site first (Caddy creates it on the first HTTPS request)")
		}
		return err
	}
	defer in.Close()
	out, err := os.Create(ExportCATarget(dest))
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ExportCAName is the file name ExportCA uses inside a directory.
const ExportCAName = "mullion-root-ca.crt"

// ExportCATarget is the file ExportCA writes for dest.
func ExportCATarget(dest string) string {
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		return filepath.Join(dest, ExportCAName)
	}
	return dest
}
