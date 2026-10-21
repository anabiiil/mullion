package redis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderConfig(t *testing.T) {
	conf := RenderConfig(ConfOptions{Port: 26379})
	for _, want := range []string{
		"bind 127.0.0.1\n", "port 26379\n", "dir data\n", "dbfilename dump.rdb\n",
		"appendonly no\n", "maxmemory 512mb\n", "maxmemory-policy noeviction\n",
		"daemonize no\n", `logfile ""` + "\n", "protected-mode yes\n", "databases 16\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("redis.conf lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "requirepass") {
		t.Error("no password by default")
	}
	// No absolute paths — the Windows (MSYS2) build can't read C:\ ones.
	for _, l := range strings.Split(conf, "\n") {
		if strings.Contains(l, `:\`) || strings.Contains(l, " /") {
			t.Errorf("absolute path in %q", l)
		}
	}
	if c := RenderConfig(ConfOptions{}); !strings.Contains(c, "port 6379\n") {
		t.Error("port 0 should render the default")
	}
	if c := RenderConfig(ConfOptions{Port: 1, Password: `p"w\x`}); !strings.Contains(c, `requirepass "p\"w\\x"`) {
		t.Errorf("password not quoted: %s", c)
	}
}

func TestParseHashes(t *testing.T) {
	text := `Hashes of Redis releases
hash redis-7.2.16.tar.gz sha256 960a8ec15e34ff40e57ff16837b26b33bd81f2da6d24497bb63de532a323a18e http://download.redis.io/releases/redis-7.2.16.tar.gz
hash redis-8.10.2.tar.gz sha256 b9ffee226b5eecdba98a679260dad764b2a4ebd90dce4ad5ac9e9f3eef9c02b3 http://download.redis.io/releases/redis-8.10.2.tar.gz
hash redis-8.12-rc1.tar.gz sha256 1111111111111111111111111111111111111111111111111111111111111111 http://download.redis.io/releases/redis-8.12-rc1.tar.gz
hash redis-8.8.3.tar.gz sha256 13dbcfc6107ab8b6ab2a4f4582678143d5b2fd03ba38611b810359564cfe8a3c http://download.redis.io/releases/redis-8.8.3.tar.gz
`
	rel, err := ParseHashes(text)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "8.10.2" || rel.SHA256 != "b9ffee226b5eecdba98a679260dad764b2a4ebd90dce4ad5ac9e9f3eef9c02b3" ||
		rel.URL != "https://download.redis.io/releases/redis-8.10.2.tar.gz" {
		t.Errorf("latest = %+v", rel)
	}
	if _, err := ParseHashes("nothing here"); err == nil {
		t.Error("no releases should fail")
	}
}

func TestPickWindowsAsset(t *testing.T) {
	rel := WindowsRelease{Tag: "8.10.2", Assets: []Asset{
		{Name: "Redis-8.10.2-Windows-x64-cygwin-with-Service.zip"},
		{Name: "Redis-8.10.2-Windows-x64-cygwin.zip"},
		{Name: "Redis-8.10.2-Windows-x64-msys2-with-Service.zip"},
		{Name: "Redis-8.10.2-Windows-x64-msys2.zip", Digest: "sha256:abc"},
	}}
	a, err := PickWindowsAsset(rel)
	if err != nil || a.Name != "Redis-8.10.2-Windows-x64-msys2.zip" || a.Digest != "sha256:abc" {
		t.Errorf("picked %+v %v", a, err)
	}
	rel.Assets = rel.Assets[:2]
	if a, err := PickWindowsAsset(rel); err != nil || a.Name != "Redis-8.10.2-Windows-x64-cygwin.zip" {
		t.Errorf("fallback picked %+v %v", a, err)
	}
	rel.Assets = rel.Assets[:1]
	if _, err := PickWindowsAsset(rel); err == nil {
		t.Error("only the service build: should fail")
	}
	if (WindowsRelease{Tag: "v8.8.3"}).Version() != "8.8.3" {
		t.Error("tag v-prefix not stripped")
	}
}

func TestNewer(t *testing.T) {
	if !Newer("8.10.2", "8.8.3") || Newer("8.8.3", "8.10.2") || Newer("8.10.2", "8.10.2") {
		t.Error("Newer compares numerically")
	}
}

func TestInstalledAndRemove(t *testing.T) {
	home := t.TempDir()
	paths := pmdirPaths(home)
	for _, v := range []string{"8.8.3", "8.10.2"} {
		exe := ServerExe(paths, v)
		os.MkdirAll(filepath.Dir(exe), 0o755)
		os.WriteFile(exe, []byte("x"), 0o755)
	}
	os.MkdirAll(filepath.Join(BaseDir(paths), "8.9.9"), 0o755) // no binary
	os.MkdirAll(DataDir(paths), 0o755)
	os.WriteFile(DumpFile(paths), []byte("rdb"), 0o644)
	if got := Installed(paths); strings.Join(got, ",") != "8.10.2,8.8.3" {
		t.Errorf("Installed = %v", got)
	}
	if err := Remove(paths, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DumpFile(paths)); err != nil {
		t.Error("keepData must keep dump.rdb")
	}
	if len(Installed(paths)) != 0 {
		t.Error("versions should be gone")
	}
	if err := Remove(paths, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(BaseDir(paths)); !os.IsNotExist(err) {
		t.Error("full remove should delete the folder")
	}
	if err := RemoveVersion(paths, "../x"); err == nil {
		t.Error("a bad version must be refused")
	}
}
