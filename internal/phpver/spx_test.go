package phpver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pm/internal/pmdir"
)

func TestSpxClangArgs(t *testing.T) {
	args := spxClangArgs("/b/php-8.4.1", "/b/php-spx", "/b/cfg", "/sdk", "/b/spx.so")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-DHAVE_CONFIG_H", "-DCOMPILE_DL_SPX=1", "-std=gnu90", "-Wno-typedef-redefinition",
		"-I/b/cfg", "-I" + filepath.Join("/b/php-8.4.1", "main"), "-I" + filepath.Join("/b/php-8.4.1", "Zend"),
		"-I" + filepath.Join("/b/php-8.4.1", "TSRM"), "-isysroot /sdk",
		"-bundle -undefined dynamic_lookup -o /b/spx.so",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in: %s", want, joined)
		}
	}
	if strings.Contains(joined, "-Werror") {
		t.Error("-Werror would let a compiler warning fail the build")
	}
	if args[len(args)-1] != "-lz" {
		t.Errorf("zlib must be linked last: %v", args[len(args)-1])
	}
	for _, src := range spxSources {
		if !strings.Contains(joined, filepath.Join("/b/php-spx", "src", src)) {
			t.Errorf("source %s not compiled", src)
		}
	}
	if len(spxSources) != 17 {
		t.Errorf("SPX %s's config.m4 lists 17 sources, have %d", SpxVersion, len(spxSources))
	}
	if strings.Contains(strings.Join(spxClangArgs("/p", "/s", "/c", "", "/o"), " "), "-isysroot") {
		t.Error("-isysroot without an SDK")
	}
}

func TestSpxConfigH(t *testing.T) {
	h := spxConfigH(`/Users/me/.mullion/lib/spx-0.4.22/web-ui`)
	if !strings.Contains(h, "#define HAVE_SPX 1\n") || !strings.Contains(h, `#define SPX_HTTP_UI_ASSETS_DIR "/Users/me/.mullion/lib/spx-0.4.22/web-ui"`) {
		t.Fatalf("config.h:\n%s", h)
	}
	if strings.Contains(spxIniComment, "=") {
		t.Fatal("the php.ini comment must not contain '=' (parseExtLine would read it)")
	}
}

func TestSpxLoadedAndWanted(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	for _, v := range []string{"8.4.1", "8.4.2", "8.3.9"} {
		os.MkdirAll(filepath.Join(paths.PhpVersionDir(v), "ext"), 0o755)
		os.WriteFile(filepath.Join(paths.PhpVersionDir(v), PhpExeName), nil, 0o755)
	}
	if SpxLoaded(paths, "8.4.1") {
		t.Fatal("loaded without spx.so")
	}
	so := SpxSoPath(paths, "8.4.1")
	os.WriteFile(so, nil, 0o755)
	ini := filepath.Join(paths.PhpVersionDir("8.4.1"), "php.ini")
	os.WriteFile(ini, []byte(";extension=\""+so+"\"\n"), 0o644)
	if SpxLoaded(paths, "8.4.1") {
		t.Fatal("a commented-out line counts as loaded")
	}
	if err := setSharedExtLine(ini, so, spxIniComment, true); err != nil {
		t.Fatal(err)
	}
	if !SpxLoaded(paths, "8.4.1") {
		data, _ := os.ReadFile(ini)
		t.Fatalf("not loaded after enabling:\n%s", data)
	}
	// A new patch of the same branch wants SPX; another branch doesn't.
	if !spxWanted(paths, "8.4.2") {
		t.Fatal("8.4.2 should get SPX rebuilt (8.4.1 has it)")
	}
	if spxWanted(paths, "8.3.9") || spxWanted(paths, "8.4.1") {
		t.Fatal("spxWanted for a version without a sibling that has SPX")
	}
}

func TestMigrateRepointsSpx(t *testing.T) {
	paths := pmdir.Paths{Home: t.TempDir()}
	oldDir, newDir := paths.PhpVersionDir("8.4.1"), paths.PhpVersionDir("8.4.2")
	for _, d := range []string{oldDir, newDir} {
		os.MkdirAll(filepath.Join(d, "ext"), 0o755)
	}
	oldSo := SpxSoPath(paths, "8.4.1")
	os.WriteFile(filepath.Join(oldDir, "php.ini"), []byte("; "+spxIniComment+"\nextension=\""+oldSo+"\"\n"), 0o644)
	// The update's install rebuilt spx.so for the new patch.
	os.WriteFile(SpxSoPath(paths, "8.4.2"), nil, 0o755)
	if _, err := MigrateIni(paths, "8.4.1", "8.4.2"); err != nil {
		t.Fatal(err)
	}
	if !SpxLoaded(paths, "8.4.2") {
		data, _ := os.ReadFile(filepath.Join(newDir, "php.ini"))
		t.Fatalf("spx not loaded by the new patch:\n%s", data)
	}
}
