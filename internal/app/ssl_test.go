package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pm/internal/config"
)

// testCA generates a self-signed root like Caddy's.
func testCA(t *testing.T, cn string, notAfter time.Time) ([]byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(42),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), strings.ToUpper(hex.EncodeToString(sum[:]))
}

func TestParseCA(t *testing.T) {
	expires := time.Date(2036, 9, 1, 12, 0, 0, 0, time.UTC)
	pemBytes, thumb := testCA(t, "Caddy Local Authority - 2026 ECC Root", expires)
	ca, err := parseCA(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ca.subject != "Caddy Local Authority - 2026 ECC Root" || !ca.expires.Equal(expires) || ca.thumbprint != thumb {
		t.Errorf("parsed = %+v, want thumb %s", ca, thumb)
	}
	if _, err := parseCA([]byte("junk")); err == nil {
		t.Error("junk parsed")
	}
}

// sslTestApp points Caddy's data dir (XDG_DATA_HOME, honored by
// caddy.RootCertPath) at a sandbox and fakes the trust-store query.
func sslTestApp(t *testing.T, trusted bool) (*App, string) {
	t.Helper()
	a, _ := portsTestApp(t)
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	old := caTrusted
	caTrusted = func(string, string) bool { return trusted }
	t.Cleanup(func() { caTrusted = old })
	return a, filepath.Join(xdg, "caddy", "pki", "authorities", "local", "root.crt")
}

func TestSSLInfo(t *testing.T) {
	a, root := sslTestApp(t, true)
	a.State.Sites = []config.Site{{Name: "a", Secure: true}, {Name: "b"}, {Name: "c", Secure: true}}

	info := a.SSLInfo()
	if info.CAExists || info.Trusted || info.CAPath != root || info.SecureSites != 2 || info.TotalSites != 3 {
		t.Errorf("no CA yet: %+v", info)
	}
	if err := a.ExportCA(t.TempDir()); err == nil {
		t.Error("exported a CA that doesn't exist")
	}

	pemBytes, _ := testCA(t, "Caddy Local Authority - 2026 ECC Root", time.Date(2036, 9, 1, 0, 0, 0, 0, time.UTC))
	os.MkdirAll(filepath.Dir(root), 0o700)
	os.WriteFile(root, pemBytes, 0o600)
	info = a.SSLInfo()
	if !info.CAExists || !info.Trusted || info.CASubject != "Caddy Local Authority - 2026 ECC Root" || info.CAExpires != "2036-09-01" {
		t.Errorf("with CA: %+v", info)
	}

	// Export into a directory and to a file path.
	dir := t.TempDir()
	if err := a.ExportCA(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ExportCAName)); string(b) != string(pemBytes) {
		t.Error("exported bytes differ (directory)")
	}
	file := filepath.Join(dir, "ca.pem")
	if err := a.ExportCA(file); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != string(pemBytes) {
		t.Error("exported bytes differ (file)")
	}
}

func TestSecureAll(t *testing.T) {
	a, _ := sslTestApp(t, false)
	a.State.Sites = []config.Site{{Name: "a", Secure: true}, {Name: "b"}}
	if err := a.SecureAll(true); err != nil {
		t.Fatal(err)
	}
	st, _ := config.Load(a.Paths)
	for _, s := range st.Sites {
		if !s.Secure {
			t.Errorf("%s not secured", s.Name)
		}
	}
	if err := a.SecureAll(false); err != nil {
		t.Fatal(err)
	}
	if a.SSLInfo().SecureSites != 0 {
		t.Error("sites still secure")
	}
}
