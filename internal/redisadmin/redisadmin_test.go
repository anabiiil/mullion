package redisadmin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestPredisTag(t *testing.T) {
	cases := map[string]string{
		`{"require":{"predis/predis":"v2.3.0","ext-json":"*"}}`: "v2.3.0",
		`{"require":{"predis/predis":"^2.3"}}`:                  "v2.3.0",
		`{"require":{"predis/predis":"~3.1.4"}}`:                "v3.1.4",
	}
	for in, want := range cases {
		if got, err := PredisTag([]byte(in)); err != nil || got != want {
			t.Errorf("PredisTag(%s) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`{}`, `{"require":{"predis/predis":"dev-main"}}`, `nope`} {
		if _, err := PredisTag([]byte(bad)); err == nil {
			t.Errorf("PredisTag(%s) should fail", bad)
		}
	}
}

func TestConfigPHP(t *testing.T) {
	c := ConfigPHP(26379, "")
	for _, want := range []string{"'port'      => 26379,", "'allow_no_auth' => true,", "'host'      => '127.0.0.1',", "'databases' => 16,"} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "'auth'") {
		t.Error("no auth without a password")
	}
	c = ConfigPHP(6379, `it's\x`)
	if !strings.Contains(c, `'auth'      => 'it\'s\\x',`) {
		t.Errorf("password not escaped:\n%s", c)
	}
	// It must be valid PHP when a php binary is around.
	if php, err := exec.LookPath("php"); err == nil {
		f := filepath.Join(t.TempDir(), "c.php")
		os.WriteFile(f, []byte(c), 0o644)
		if out, err := exec.Command(php, "-n", "-l", f).CombinedOutput(); err != nil {
			t.Errorf("php -l: %v %s", err, out)
		}
	}
}

func TestURLAndWriteConfig(t *testing.T) {
	if URL("test", -1) != "https://redis.test/" || URL("test", 3) != "https://redis.test/?s=0&d=3" {
		t.Error("URL")
	}
	paths := pmdir.Paths{Home: t.TempDir()}
	if err := WriteConfig(paths, 1, ""); err != nil {
		t.Error("WriteConfig on a missing install should be a no-op")
	}
	os.MkdirAll(filepath.Join(Dir(paths), "includes"), 0o755)
	if err := WriteConfig(paths, 6390, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(Dir(paths), "includes", "config.inc.php"))
	if !strings.Contains(string(b), "=> 6390,") {
		t.Errorf("config not written:\n%s", b)
	}
	if Installed(paths) {
		t.Error("not installed without index.php and vendor")
	}
}
