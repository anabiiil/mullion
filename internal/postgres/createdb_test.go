package postgres

import "testing"

const sampleLocales = "C\t\nPOSIX\t\nen_US.UTF-8\tUTF8\nar_EG.ISO8859-6\tISO_8859_6\nde_DE.ISO8859-1\tLATIN1\n" +
	"en_US.UTF-8\tUTF8\nbad'locale\tUTF8\n=UTF8\tC\n"

func TestParseLocales(t *testing.T) {
	info := parseLocales(sampleLocales)
	if info.DefaultEncoding != "UTF8" || info.DefaultLocale != "C" {
		t.Errorf("defaults = %q/%q", info.DefaultEncoding, info.DefaultLocale)
	}
	var names []string
	for _, l := range info.Locales {
		names = append(names, l.Name)
	}
	want := []string{"C", "POSIX", "en_US.UTF-8", "ar_EG.ISO8859-6", "de_DE.ISO8859-1"}
	if len(names) != len(want) {
		t.Fatalf("locales = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("locales = %v, want %v", names, want)
		}
	}
	if len(info.Encodings) == 0 || info.Encodings[0] != "UTF8" {
		t.Errorf("encodings = %v", info.Encodings)
	}
}

func TestResolveLocale(t *testing.T) {
	info := parseLocales(sampleLocales)
	ok := []struct{ in, want CreateOptions }{
		{CreateOptions{"utf-8", ""}, CreateOptions{"UTF8", ""}},
		{CreateOptions{"", "en_US.UTF-8"}, CreateOptions{"", "en_US.UTF-8"}},
		{CreateOptions{"LATIN1", "de_DE.ISO8859-1"}, CreateOptions{"LATIN1", "de_DE.ISO8859-1"}},
		{CreateOptions{"WIN1256", "C"}, CreateOptions{"WIN1256", "C"}},
		{CreateOptions{"iso-8859-6", "ar_EG.ISO8859-6"}, CreateOptions{"ISO_8859_6", "ar_EG.ISO8859-6"}},
	}
	for _, c := range ok {
		got, err := info.Resolve(c.in)
		if err != nil || got != c.want {
			t.Errorf("Resolve(%+v) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []CreateOptions{
		{"SJIS", ""},                   // client-only encoding
		{"", "xx_YY.UTF-8"},            // not on the server
		{"LATIN1", "en_US.UTF-8"},      // locale/encoding mismatch
		{"", "de_DE.ISO8859-1"},        // LATIN1 locale, cluster UTF8
		{"UTF8'; DROP DATABASE x", ""}, // injection
		{"", "C' TEMPLATE x --"},
	} {
		if _, err := info.Resolve(bad); err == nil {
			t.Errorf("Resolve(%+v) accepted", bad)
		}
	}
}

func TestCreateDatabaseSQL(t *testing.T) {
	cases := []struct {
		opts CreateOptions
		want string
	}{
		{CreateOptions{}, `CREATE DATABASE "shop" ENCODING 'UTF8';`},
		{CreateOptions{"WIN1256", "C"}, `CREATE DATABASE "shop" ENCODING 'WIN1256' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;`},
		{CreateOptions{"", "en_US.UTF-8"}, `CREATE DATABASE "shop" ENCODING 'UTF8' LC_COLLATE 'en_US.UTF-8' LC_CTYPE 'en_US.UTF-8' TEMPLATE template0;`},
	}
	for _, c := range cases {
		got, err := createDatabaseSQL("shop", c.opts)
		if err != nil || got != c.want {
			t.Errorf("createDatabaseSQL(%+v) = %q, %v; want %q", c.opts, got, err, c.want)
		}
	}
	for _, bad := range []CreateOptions{{"NOPE", ""}, {"UTF8", "C'"}} {
		if _, err := createDatabaseSQL("shop", bad); err == nil {
			t.Errorf("createDatabaseSQL(%+v) accepted", bad)
		}
	}
	if _, err := createDatabaseSQL(`sh"op`, CreateOptions{}); err == nil {
		t.Error("bad name accepted")
	}
}

func TestParseDetails(t *testing.T) {
	dbs := parseDetails("shop\tUTF8\tC\nlegacy\tLATIN1\tde_DE.ISO8859-1\npostgres\tUTF8\tC\n\n")
	if len(dbs) != 2 || dbs[0] != (DatabaseInfo{"shop", "UTF8", "C"}) || dbs[1].Encoding != "LATIN1" {
		t.Errorf("parseDetails = %+v", dbs)
	}
}
