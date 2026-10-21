package mssql

import (
	"strings"
	"testing"
)

const sampleCollations = "0\tSQL_Latin1_General_CP1_CI_AS\n1\tAlbanian_CI_AS\n1\tArabic_CI_AS\n" +
	"1\tSQL_Latin1_General_CP1_CI_AS\n1\tLatin1_General_100_CI_AS_SC_UTF8\n1\tbad name\n1\tZulu_CI_AS\n"

func TestParseCollations(t *testing.T) {
	info := parseCollations(sampleCollations)
	if info.Default != "SQL_Latin1_General_CP1_CI_AS" {
		t.Errorf("default = %q", info.Default)
	}
	got := strings.Join(info.Collations, ",")
	want := "SQL_Latin1_General_CP1_CI_AS,Latin1_General_100_CI_AS_SC_UTF8,Arabic_CI_AS,Albanian_CI_AS,Zulu_CI_AS"
	if got != want {
		t.Errorf("collations = %s\nwant          %s", got, want)
	}
}

func TestResolveCollation(t *testing.T) {
	info := parseCollations(sampleCollations)
	if c, err := info.Resolve("arabic_ci_as"); err != nil || c != "Arabic_CI_AS" {
		t.Errorf("Resolve = %q, %v", c, err)
	}
	for _, bad := range []string{"Klingon_CI_AS", "Arabic_CI_AS; DROP DATABASE x", ""} {
		if _, err := info.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) accepted", bad)
		}
	}
}

func TestCreateDatabaseWithSQL(t *testing.T) {
	if got, err := createDatabaseWithSQL("shop", ""); err != nil || got != "CREATE DATABASE [shop];" {
		t.Errorf("no collation: %q, %v", got, err)
	}
	if got, err := createDatabaseWithSQL("shop", "Arabic_100_CI_AS_SC_UTF8"); err != nil ||
		got != "CREATE DATABASE [shop] COLLATE Arabic_100_CI_AS_SC_UTF8;" {
		t.Errorf("collation: %q, %v", got, err)
	}
	for _, c := range [][2]string{{"shop", "x; DROP"}, {"master", ""}, {"sh]op", ""}} {
		if _, err := createDatabaseWithSQL(c[0], c[1]); err == nil {
			t.Errorf("createDatabaseWithSQL(%q) accepted", c)
		}
	}
}

func TestParseDetails(t *testing.T) {
	dbs := parseDetails("shop\tSQL_Latin1_General_CP1_CI_AS\narabic\tArabic_CI_AS\noffline\tNULL\n")
	if len(dbs) != 3 || dbs[0] != (DatabaseInfo{"arabic", "Arabic_CI_AS"}) || dbs[1].Collation != "" {
		t.Errorf("parseDetails = %+v", dbs)
	}
}
