// Package sql is flowmap's tokenizer-grade SQL normalizer (canon spec §3.4,
// §8.3). db.statement is the most volatile gated value — literals, ids, and
// driver placeholders all vary per run — so the normalizer strips it down to a
// structural form: literals and placeholders become "?", IN-lists and multi-row
// inserts collapse, whitespace is single-spaced, and keywords are upper-cased.
// It also reports the operation and primary table, which the canonical op key is
// keyed on so a span's identity barely depends on the statement text at all.
package sql

import (
	"regexp"
	"strings"
)

// Normalized is the structural reduction of a SQL statement.
type Normalized struct {
	Statement string // e.g. "SELECT name , income FROM applicants WHERE id = ?"
	Operation string // SELECT|INSERT|UPDATE|DELETE|... (upper-case); "" if unknown
	Table     string // primary table, lower-cased; "" if not found
}

var keywords = map[string]bool{
	"select": true, "insert": true, "update": true, "delete": true, "from": true,
	"into": true, "where": true, "values": true, "set": true, "and": true, "or": true,
	"in": true, "join": true, "left": true, "right": true, "inner": true, "outer": true,
	"on": true, "as": true, "order": true, "by": true, "group": true, "having": true,
	"limit": true, "offset": true, "returning": true, "null": true, "is": true, "not": true,
}

// Normalize tokenizes raw SQL into its canonical structural form.
func Normalize(raw string) Normalized {
	toks := tokenize(raw)
	out := assemble(toks)
	op, table := operationAndTable(toks)
	return Normalized{Statement: out, Operation: op, Table: table}
}

// tokenize splits raw SQL into tokens, replacing every literal and driver
// placeholder with the single placeholder token "?". Keywords are upper-cased;
// other identifiers keep their (lower-cased) form so table names stay stable.
func tokenize(raw string) []string {
	var toks []string
	i := 0
	n := len(raw)
	for i < n {
		c := raw[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'' || c == '"':
			// String literal: scan to the matching quote, honoring '' escapes.
			q := c
			i++
			for i < n {
				if raw[i] == q {
					if i+1 < n && raw[i+1] == q {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			toks = append(toks, "?")
		case c == '$' || c == ':' || c == '@':
			// Driver placeholder ($1, :name, @p1): consume the run.
			i++
			for i < n && (isIdent(raw[i]) || isDigit(raw[i])) {
				i++
			}
			toks = append(toks, "?")
		case c == '?':
			toks = append(toks, "?")
			i++
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(raw[i+1])):
			for i < n && (isDigit(raw[i]) || raw[i] == '.') {
				i++
			}
			toks = append(toks, "?")
		case isIdent(c):
			start := i
			for i < n && (isIdent(raw[i]) || isDigit(raw[i])) {
				i++
			}
			word := raw[start:i]
			if keywords[strings.ToLower(word)] {
				toks = append(toks, strings.ToUpper(word))
			} else {
				toks = append(toks, strings.ToLower(word))
			}
		default:
			toks = append(toks, string(c))
			i++
		}
	}
	return toks
}

var (
	inList   = regexp.MustCompile(`\(\s*\?(\s*,\s*\?)*\s*\)`)
	multiRow = regexp.MustCompile(`\(\?\)(\s*,\s*\(\?\))+`)
	wsRun    = regexp.MustCompile(`\s+`)
)

// assemble joins tokens with single spaces and collapses parenthesized
// placeholder lists (IN (?, ?, ?) → IN (?)) and multi-row VALUES groups
// ((?), (?) → (?)), so cardinality in the statement does not perturb the golden.
func assemble(toks []string) string {
	out := strings.Join(toks, " ")
	out = inList.ReplaceAllString(out, "(?)")
	out = multiRow.ReplaceAllString(out, "(?)")
	out = wsRun.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

// operationAndTable derives the statement's verb and primary table from the
// token stream.
func operationAndTable(toks []string) (op, table string) {
	if len(toks) == 0 {
		return "", ""
	}
	op = toks[0]
	switch op {
	case "INSERT", "DELETE":
		table = identAfter(toks, map[string]bool{"INTO": true, "FROM": true})
	case "UPDATE":
		table = updateTable(toks)
	case "SELECT":
		table = identAfter(toks, map[string]bool{"FROM": true})
	default:
		return op, ""
	}
	return op, table
}

// updateQualifiers are dialect qualifier words that can sit between UPDATE and
// the target table (Postgres ONLY; MySQL LOW_PRIORITY / IGNORE). They tokenize
// as bare identifiers, so they must be skipped to reach the real table.
var updateQualifiers = map[string]bool{"only": true, "low_priority": true, "ignore": true}

// updateTable returns the target table of an UPDATE, skipping any leading
// qualifier words so "UPDATE ONLY loans SET ..." yields "loans", not "only".
func updateTable(toks []string) string {
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if updateQualifiers[t] {
			continue
		}
		if t != "" && isIdent(t[0]) && t == strings.ToLower(t) {
			return t
		}
		break
	}
	return ""
}

// identAfter returns the first non-keyword identifier token following any of the
// given keyword tokens.
func identAfter(toks []string, after map[string]bool) string {
	for i := 0; i < len(toks)-1; i++ {
		if after[toks[i]] {
			cand := toks[i+1]
			if cand != "" && isIdent(cand[0]) && cand == strings.ToLower(cand) {
				return cand
			}
		}
	}
	return ""
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdent(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
