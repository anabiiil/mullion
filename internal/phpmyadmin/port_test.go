package phpmyadmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/mysql"
	"pm/internal/pmdir"
)

func TestRenderConfigPort(t *testing.T) {
	conf := renderConfig("s3cr3t", "", 3307)
	if !strings.Contains(conf, "$cfg['Servers'][$i]['port'] = '3307';") {
		t.Errorf("port missing:\n%s", conf)
	}
	if !strings.Contains(conf, "AllowNoPassword'] = true") {
		t.Errorf("empty password must allow no password:\n%s", conf)
	}
}

// A port change reaches an existing config: ensureConfig rewrites it when
// the port differs and leaves it alone otherwise; RefreshConfig always
// rewrites an installed phpMyAdmin.
func TestConfigFollowsPort(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	dir := paths.PhpMyAdminDir()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "index.php"), []byte("<?php"), 0o644)
	conf := filepath.Join(dir, "config.inc.php")
	old := mysql.Port
	t.Cleanup(func() { mysql.Port = old })

	mysql.Port = 3306
	if err := ensureConfig(dir, ""); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(conf)
	if err := ensureConfig(dir, ""); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(conf)
	if string(first) != string(again) {
		t.Error("ensureConfig rewrote an up-to-date config")
	}

	mysql.Port = 3399
	if err := ensureConfig(dir, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(conf)
	if !strings.Contains(string(b), "['port'] = '3399'") {
		t.Errorf("ensureConfig kept a stale port:\n%s", b)
	}

	mysql.Port = 3400
	if err := RefreshConfig(paths, "pw"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(conf)
	if !strings.Contains(string(b), "['port'] = '3400'") || !strings.Contains(string(b), "'password'] = 'pw'") {
		t.Errorf("RefreshConfig:\n%s", b)
	}
}
