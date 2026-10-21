package postgres

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"pm/internal/pmdir"
)

// CreateOptions are the per-database settings CREATE DATABASE takes.
// Locale sets both LC_COLLATE and LC_CTYPE. Empty fields keep the
// cluster's (UTF8 / C — see initdb's flags in Init).
type CreateOptions struct {
	Encoding string `json:"encoding"`
	Locale   string `json:"locale"`
}

// Locale is one libc locale the server knows (from pg_collation) and
// the encoding it works with ("" = any, like C and POSIX).
type Locale struct {
	Name     string `json:"name"`
	Encoding string `json:"encoding"`
}

// LocaleInfo is what the server offers for new databases.
type LocaleInfo struct {
	Encodings       []string `json:"encodings"`
	Locales         []Locale `json:"locales"`
	DefaultEncoding string   `json:"defaultEncoding"`
	DefaultLocale   string   `json:"defaultLocale"`
}

// ServerEncodings are the character sets a PostgreSQL database can be
// stored in — the "Server = Yes" rows of
// https://www.postgresql.org/docs/current/multibyte.html (client-only
// ones like SJIS, BIG5, GBK can't be a database's encoding). Common
// ones first.
var ServerEncodings = []string{
	"UTF8", "LATIN1", "WIN1256", "SQL_ASCII", "WIN1252", "LATIN9", "ISO_8859_6",
	"EUC_CN", "EUC_JIS_2004", "EUC_JP", "EUC_KR", "EUC_TW",
	"ISO_8859_5", "ISO_8859_7", "ISO_8859_8", "KOI8R", "KOI8U",
	"LATIN2", "LATIN3", "LATIN4", "LATIN5", "LATIN6", "LATIN7", "LATIN8", "LATIN10",
	"MULE_INTERNAL", "WIN866", "WIN874", "WIN1250", "WIN1251", "WIN1253",
	"WIN1254", "WIN1255", "WIN1257", "WIN1258",
}

// localeRe is what a libc / Windows locale name looks like:
// en_US.UTF-8, ar_EG.ISO8859-6, de_DE@euro, en-US, "English_United
// States.1252". No quotes, so it can't break out of the literal it is
// put in (which is quoted anyway).
var localeRe = regexp.MustCompile(`^[A-Za-z0-9_.@ ()-]{1,128}$`)

// localesSQL lists libc locales (collprovider 'c') and their encoding,
// tab-separated; -1 (any encoding) comes out as "".
const localesSQL = "SELECT DISTINCT collcollate || chr(9) || CASE WHEN collencoding = -1 THEN '' ELSE pg_encoding_to_char(collencoding) END " +
	"FROM pg_collation WHERE collprovider = 'c' AND collcollate IS NOT NULL;\n" +
	"SELECT '=' || pg_encoding_to_char(encoding) || chr(9) || datcollate FROM pg_database WHERE datname = 'template0';"

// parseLocales reads localesSQL's output.
func parseLocales(out string) LocaleInfo {
	info := LocaleInfo{Encodings: ServerEncodings}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" {
			continue
		}
		name, enc, _ := strings.Cut(line, "\t")
		if strings.HasPrefix(name, "=") {
			info.DefaultEncoding, info.DefaultLocale = strings.TrimPrefix(name, "="), enc
			continue
		}
		if !localeRe.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		info.Locales = append(info.Locales, Locale{Name: name, Encoding: enc})
	}
	for _, l := range []string{info.DefaultLocale, "C", "POSIX"} {
		if l != "" && localeRe.MatchString(l) && !seen[l] {
			seen[l] = true
			info.Locales = append(info.Locales, Locale{Name: l})
		}
	}
	// C / POSIX first, then UTF-8 locales, then the rest by name.
	weight := func(l Locale) int {
		switch {
		case l.Name == "C" || l.Name == "POSIX":
			return 0
		case l.Encoding == "UTF8":
			return 1
		}
		return 2
	}
	sort.SliceStable(info.Locales, func(i, j int) bool {
		a, b := info.Locales[i], info.Locales[j]
		if weight(a) != weight(b) {
			return weight(a) < weight(b)
		}
		return a.Name < b.Name
	})
	return info
}

// Locales asks the running server which locales it can create
// databases with.
func Locales(paths pmdir.Paths, version string) (LocaleInfo, error) {
	out, err := psql(paths, version, Password, localesSQL)
	if err != nil {
		return LocaleInfo{}, fmt.Errorf("listing locales: %w", err)
	}
	return parseLocales(out), nil
}

// Resolve checks opts against what the server offers (canonicalizing
// case) and that the locale works with the encoding: a libc locale
// is tied to one encoding, except C and POSIX.
func (info LocaleInfo) Resolve(opts CreateOptions) (CreateOptions, error) {
	out := CreateOptions{}
	if e := strings.TrimSpace(opts.Encoding); e != "" {
		norm := strings.ToUpper(strings.ReplaceAll(e, "-", ""))
		for _, s := range info.Encodings {
			if strings.ReplaceAll(s, "_", "") == strings.ReplaceAll(norm, "_", "") {
				out.Encoding = s
			}
		}
		if out.Encoding == "" {
			return out, fmt.Errorf("PostgreSQL has no server encoding %q", e)
		}
	}
	if l := strings.TrimSpace(opts.Locale); l != "" {
		var found *Locale
		for i := range info.Locales {
			if info.Locales[i].Name == l {
				found = &info.Locales[i]
				break
			}
		}
		if found == nil {
			return out, fmt.Errorf("the server has no locale %q (see `locale -a`)", l)
		}
		out.Locale = found.Name
		enc := out.Encoding
		if enc == "" {
			enc = info.DefaultEncoding
		}
		if found.Encoding != "" && enc != "" && found.Encoding != enc {
			return out, fmt.Errorf("locale %s needs encoding %s, not %s", found.Name, found.Encoding, enc)
		}
	}
	return out, nil
}

// createDatabaseSQL renders CREATE DATABASE. A non-default encoding or
// locale needs TEMPLATE template0: template1's are fixed to the
// cluster's.
func createDatabaseSQL(name string, opts CreateOptions) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	enc := opts.Encoding
	if enc == "" {
		enc = "UTF8"
	}
	known := false
	for _, e := range ServerEncodings {
		known = known || e == enc
	}
	if !known {
		return "", fmt.Errorf("unknown encoding %q", enc)
	}
	stmt := "CREATE DATABASE " + quoteIdent(name) + " ENCODING " + quoteLiteral(enc)
	if opts.Locale != "" {
		if !localeRe.MatchString(opts.Locale) {
			return "", fmt.Errorf("invalid locale %q", opts.Locale)
		}
		stmt += " LC_COLLATE " + quoteLiteral(opts.Locale) + " LC_CTYPE " + quoteLiteral(opts.Locale)
	}
	if opts.Encoding != "" || opts.Locale != "" {
		stmt += " TEMPLATE template0"
	}
	return stmt + ";", nil
}

// DatabaseInfo is one user database with its encoding and collation.
type DatabaseInfo struct {
	Name     string `json:"name"`
	Encoding string `json:"encoding"`
	Collate  string `json:"collate"`
}

const detailsSQL = "SELECT datname || chr(9) || pg_encoding_to_char(encoding) || chr(9) || datcollate FROM pg_database " +
	"WHERE NOT datistemplate AND datname <> 'postgres' ORDER BY datname;"

func parseDetails(out string) []DatabaseInfo {
	var dbs []DatabaseInfo
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		cols := strings.Split(strings.TrimRight(line, "\r"), "\t")
		name := strings.TrimSpace(cols[0])
		if name == "" || systemDBs[name] {
			continue
		}
		d := DatabaseInfo{Name: name}
		if len(cols) >= 3 {
			d.Encoding, d.Collate = strings.TrimSpace(cols[1]), strings.TrimSpace(cols[2])
		}
		dbs = append(dbs, d)
	}
	return dbs
}

// UserDatabaseDetails lists the user databases with their encoding and
// collation (one query).
func UserDatabaseDetails(paths pmdir.Paths, version string) ([]DatabaseInfo, error) {
	out, err := psql(paths, version, Password, detailsSQL)
	if err != nil {
		return nil, fmt.Errorf("listing databases: %w", err)
	}
	return parseDetails(out), nil
}
