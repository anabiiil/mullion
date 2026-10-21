package watch

import (
	"net/url"
	"regexp"
	"strings"
)

// Redacted replaces a secret value.
const Redacted = "[redacted]"

// secretName matches header, query, context and column names whose
// values are secrets.
var secretName = regexp.MustCompile(`(?i)pass(word|wd|phrase)?|secret|token|api[_-]?key|authori[sz]ation|cookie|credential|private[_-]?key|otp|pin_code|cvv|card_number|x-csrf|x-xsrf`)

// IsSecretName reports whether a field called name holds a secret.
func IsSecretName(name string) bool { return secretName.MatchString(name) }

// RedactHeaders returns headers with secret ones blanked.
func RedactHeaders(h map[string][]string) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if IsSecretName(k) {
			out[k] = []string{Redacted}
			continue
		}
		out[k] = v
	}
	return out
}

// RedactQuery returns a URL query as a map with secret parameters blanked.
func RedactQuery(v url.Values) map[string][]string {
	out := make(map[string][]string, len(v))
	for k, vals := range v {
		if IsSecretName(k) {
			out[k] = []string{Redacted}
			continue
		}
		out[k] = vals
	}
	return out
}

var (
	insertCols = regexp.MustCompile(`(?is)^\s*(?:insert|replace)\b.*?\(([^)]*)\)\s*values\s*\(`)
	colBefore  = regexp.MustCompile(`(?i)([A-Za-z_]\w*)[` + "`" + `"\]]?\s*(?:=|<>|!=|<=|>=|<|>|\blike\b|\bin\s*\([^)]*)\s*$`)
)

// PlaceholderColumns maps each "?" of a SQL statement (outside quotes)
// to the column it is compared with or inserted into ("" = unknown).
func PlaceholderColumns(sql string) []string {
	var insert []string
	valuesAt := -1
	if m := insertCols.FindStringSubmatchIndex(sql); m != nil {
		for _, c := range strings.Split(sql[m[2]:m[3]], ",") {
			insert = append(insert, strings.Trim(c, " \t\r\n`\"[]"))
		}
		valuesAt = m[1] - 1
	}
	var cols []string
	var quote byte
	idx := 0
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			continue
		case '?':
		default:
			continue
		}
		if insert != nil && i > valuesAt {
			cols = append(cols, insert[idx%len(insert)])
			idx++
			continue
		}
		start := i - 80
		if start < 0 {
			start = 0
		}
		col := ""
		if m := colBefore.FindStringSubmatch(sql[start:i]); m != nil {
			col = m[1]
		}
		cols = append(cols, col)
	}
	return cols
}

// RedactBindings blanks the bindings of secret-looking columns.
func RedactBindings(sql string, bindings []any) []any {
	if len(bindings) == 0 || !secretName.MatchString(sql) {
		return bindings
	}
	cols := PlaceholderColumns(sql)
	out := make([]any, len(bindings))
	copy(out, bindings)
	for i := range out {
		if i < len(cols) && IsSecretName(cols[i]) {
			out[i] = Redacted
		}
	}
	return out
}
