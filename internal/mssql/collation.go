package mssql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"pm/internal/pmdir"
)

// CommonCollations come first in the panel's list, in this order: the
// server's long-time default, the UTF-8 one (SQL Server 2019+) and the
// Arabic ones.
var CommonCollations = []string{
	"SQL_Latin1_General_CP1_CI_AS",
	"Latin1_General_100_CI_AS_SC_UTF8",
	"Latin1_General_CI_AS",
	"Arabic_CI_AS",
	"Arabic_100_CI_AS_SC_UTF8",
}

// collationRe is the shape of every collation name; names are also
// checked against the server's list before they reach SQL.
var collationRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)

// CollationInfo is what the server offers for new databases.
type CollationInfo struct {
	Collations []string `json:"collations"`
	Default    string   `json:"default"`
}

// DatabaseInfo is one user database and its collation.
type DatabaseInfo struct {
	Name      string `json:"name"`
	Collation string `json:"collation"`
}

// collationsSQL returns "0<TAB>server default" then "1<TAB>name" for
// every collation (one round trip: on macOS each query is a docker
// exec).
const collationsSQL = "SET NOCOUNT ON; SELECT 0, CONVERT(nvarchar(128), SERVERPROPERTY('Collation')) " +
	"UNION ALL SELECT 1, name FROM sys.fn_helpcollations();"

// parseCollations reads collationsSQL's rows: common collations first
// (those the server has), then the rest by name.
func parseCollations(out string) CollationInfo {
	info := CollationInfo{}
	have := map[string]bool{}
	var rest []string
	for _, r := range parseRows(out) {
		if len(r) < 2 || !collationRe.MatchString(r[1]) {
			continue
		}
		if r[0] == "0" {
			info.Default = r[1]
			continue
		}
		if !have[r[1]] {
			have[r[1]] = true
			rest = append(rest, r[1])
		}
	}
	if info.Default != "" && !have[info.Default] {
		have[info.Default] = true
		rest = append(rest, info.Default)
	}
	common := map[string]bool{}
	for _, c := range CommonCollations {
		if have[c] {
			common[c] = true
			info.Collations = append(info.Collations, c)
		}
	}
	sort.Strings(rest)
	for _, c := range rest {
		if !common[c] {
			info.Collations = append(info.Collations, c)
		}
	}
	return info
}

// Collations asks the running server which collations it offers.
func Collations(paths pmdir.Paths) (CollationInfo, error) {
	out, err := query(paths, collationsSQL)
	if err != nil {
		return CollationInfo{}, fmt.Errorf("listing collations: %w", err)
	}
	return parseCollations(out), nil
}

// Resolve checks a collation against the server's list (case-
// insensitively) and returns its canonical spelling.
func (info CollationInfo) Resolve(collation string) (string, error) {
	collation = strings.TrimSpace(collation)
	for _, c := range info.Collations {
		if strings.EqualFold(c, collation) {
			return c, nil
		}
	}
	return "", fmt.Errorf("SQL Server has no collation %q", collation)
}

// createDatabaseWithSQL is CREATE DATABASE with an optional COLLATE.
func createDatabaseWithSQL(name, collation string) (string, error) {
	if err := ValidDatabaseName(name); err != nil {
		return "", err
	}
	if collation == "" {
		return createDatabaseSQL(name), nil
	}
	if !collationRe.MatchString(collation) {
		return "", fmt.Errorf("invalid collation %q", collation)
	}
	return "CREATE DATABASE " + quoteIdent(name) + " COLLATE " + collation + ";", nil
}

const detailsSQL = "SET NOCOUNT ON; SELECT name, collation_name FROM sys.databases WHERE database_id > 4 ORDER BY name;"

func parseDetails(out string) []DatabaseInfo {
	var dbs []DatabaseInfo
	for _, r := range parseRows(out) {
		d := DatabaseInfo{Name: r[0]}
		if len(r) >= 2 && r[1] != "NULL" {
			d.Collation = r[1]
		}
		dbs = append(dbs, d)
	}
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].Name < dbs[j].Name })
	return dbs
}

// UserDatabaseDetails lists the user databases with their collation.
func UserDatabaseDetails(paths pmdir.Paths) ([]DatabaseInfo, error) {
	out, err := query(paths, detailsSQL)
	if err != nil {
		return nil, err
	}
	return parseDetails(out), nil
}
