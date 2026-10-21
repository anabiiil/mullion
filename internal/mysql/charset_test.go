package mysql

import (
	"strings"
	"testing"
)

func sampleCharsetInfo() CharsetInfo {
	return buildCharsetInfo(
		tsvRows("latin1\tcp1252 West European\tlatin1_swedish_ci\n"+
			"utf8mb4\tUTF-8 Unicode\tutf8mb4_0900_ai_ci\n"+
			"armscii8\tARMSCII-8 Armenian\tarmscii8_general_ci\n"+
			"bad name\tx\ty\n"),
		tsvRows("latin1\tlatin1_swedish_ci\nlatin1\tlatin1_general_ci\n"+
			"utf8mb4\tutf8mb4_unicode_ci\nutf8mb4\tutf8mb4_0900_ai_ci\nutf8mb4\tutf8mb4_bin\n"+
			"utf8mb4\tutf8mb4_unicode_ci\n"+ // duplicate (MariaDB full-name rows)
			"armscii8\tarmscii8_general_ci\n"),
		tsvRows("utf8mb4\tutf8mb4_0900_ai_ci\n"),
	)
}

func TestBuildCharsetInfo(t *testing.T) {
	info := sampleCharsetInfo()
	var names []string
	for _, c := range info.Charsets {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "utf8mb4,latin1,armscii8" {
		t.Fatalf("charset order = %s (common first, invalid names dropped)", got)
	}
	u := info.Charsets[0]
	if got := strings.Join(u.Collations, ","); got != "utf8mb4_0900_ai_ci,utf8mb4_bin,utf8mb4_unicode_ci" {
		t.Errorf("utf8mb4 collations = %s (default first, deduplicated)", got)
	}
	if info.ServerCharset != "utf8mb4" || info.ServerCollation != "utf8mb4_0900_ai_ci" {
		t.Errorf("server default = %s/%s", info.ServerCharset, info.ServerCollation)
	}
}

func TestResolveCharset(t *testing.T) {
	info := sampleCharsetInfo()
	cases := []struct {
		cs, coll, wantCS, wantColl string
		bad                        bool
	}{
		{"", "", "utf8mb4", "utf8mb4_unicode_ci", false},
		{"latin1", "", "latin1", "latin1_swedish_ci", false},
		{"LATIN1", "latin1_general_ci", "latin1", "latin1_general_ci", false},
		{"", "utf8mb4_bin", "utf8mb4", "utf8mb4_bin", false},
		{"latin1", "utf8mb4_bin", "", "", true},
		{"klingon", "", "", "", true},
		{"", "utf8mb4_bin; DROP DATABASE x", "", "", true},
		{"utf8mb4`", "", "", "", true},
	}
	for _, c := range cases {
		cs, coll, err := info.Resolve(c.cs, c.coll)
		if c.bad {
			if err == nil {
				t.Errorf("Resolve(%q,%q) accepted", c.cs, c.coll)
			}
			continue
		}
		if err != nil || cs != c.wantCS || coll != c.wantColl {
			t.Errorf("Resolve(%q,%q) = %q,%q,%v; want %q,%q", c.cs, c.coll, cs, coll, err, c.wantCS, c.wantColl)
		}
	}
	// No utf8mb4 at all: fall back to the server's own default.
	srv := CharsetInfo{Charsets: []Charset{{Name: "latin1", DefaultCollation: "latin1_swedish_ci", Collations: []string{"latin1_swedish_ci"}}},
		ServerCharset: "latin1", ServerCollation: "latin1_swedish_ci"}
	if cs, coll, err := srv.Resolve("", ""); err != nil || cs != "latin1" || coll != "latin1_swedish_ci" {
		t.Errorf("server-default fallback = %q,%q,%v", cs, coll, err)
	}
}

func TestCreateDatabaseSQL(t *testing.T) {
	got, err := createDatabaseSQL("shop", "utf8mb4", "utf8mb4_unicode_ci")
	if err != nil || got != "CREATE DATABASE IF NOT EXISTS `shop` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, _ := createDatabaseSQL("shop", "latin1", ""); got != "CREATE DATABASE IF NOT EXISTS `shop` CHARACTER SET latin1;" {
		t.Errorf("charset only: %q", got)
	}
	for _, bad := range [][3]string{
		{"sh`op", "utf8mb4", ""},
		{"shop", "utf8mb4 COLLATE x", ""},
		{"shop", "utf8mb4", "utf8mb4_bin;--"},
		{"shop", "", ""},
	} {
		if _, err := createDatabaseSQL(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("createDatabaseSQL(%q) accepted", bad)
		}
	}
}

func TestParseSchemata(t *testing.T) {
	dbs := parseSchemata("information_schema\tutf8mb3\tutf8mb3_general_ci\nmysql\tutf8mb4\tutf8mb4_0900_ai_ci\n" +
		"shop\tutf8mb4\tutf8mb4_unicode_ci\nlegacy\tlatin1\tlatin1_swedish_ci\nsys\tutf8mb4\tx\n")
	if len(dbs) != 2 || dbs[0] != (DatabaseInfo{"shop", "utf8mb4", "utf8mb4_unicode_ci"}) || dbs[1].Charset != "latin1" {
		t.Errorf("parseSchemata = %+v", dbs)
	}
}
