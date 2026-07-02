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
	// The mutating verbs sqlverb.Mutating recognizes beyond INSERT/UPDATE/DELETE.
	// They must tokenize as keywords (upper-cased) so operationAndTable derives the
	// same upper-case op the static labeler emits; USING introduces a MERGE source.
	"merge": true, "upsert": true, "replace": true, "using": true,
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
// Comments (-- …, /* … */) are dropped entirely, and quoted identifiers
// ("T", `t`) are unwrapped and lower-cased so they key like the bare form.
func tokenize(raw string) []string {
	var toks []string
	i := 0
	n := len(raw)
	for i < n {
		c := raw[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < n && raw[i+1] == '-':
			// Line comment (-- …): drop to end of line (or input). Comment payloads
			// carry per-request volatile data (sqlcommenter key=value pairs) and
			// identifier-shaped fragments — neither may reach the canonical form
			// (golden churn + a hidden disclosure channel; M-24).
			i += 2
			for i < n && raw[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && raw[i+1] == '*':
			// Block comment (/* … */): PostgreSQL NESTS block comments, so track the
			// nesting depth and drop to the matching close (or to input end if
			// unterminated — conservative, an unterminated comment eats the rest,
			// never leaks it). A depth-blind scan would stop at the FIRST */ and spill
			// the outer comment's tail (`/* a /* b */ secret */` → `secret …`) into the
			// canonical statement — the exact disclosure channel M-24 closes.
			i += 2
			depth := 1
			for i < n && depth > 0 {
				if raw[i] == '/' && i+1 < n && raw[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if raw[i] == '*' && i+1 < n && raw[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
		case c == '\'':
			// String literal: scan to the matching quote, honoring '' doubling AND
			// backslash escapes (\' \\ …, MySQL/MariaDB default). A backslash consumes
			// the next byte so an escaped quote does not terminate the literal early
			// and spill its remainder out as identifier tokens — the redaction promise
			// is that NO byte from inside a quoted literal survives into the canonical
			// statement (H-1). Over-consuming (treating \ as an escape even under
			// NO_BACKSLASH_ESCAPES) only ever widens the "?", never narrows it.
			i++
			for i < n {
				if raw[i] == '\\' {
					i += 2
					continue
				}
				if raw[i] == '\'' {
					if i+1 < n && raw[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			toks = append(toks, "?")
		case c == '"' || c == '`':
			// Quoted identifier ("Applicants", `orders`): unwrap and lower-case so it
			// keys identically to the same identifier written bare and Table extraction
			// still finds it (M-24, M-25). A doubled delimiter ("" or ``) is an escaped
			// delimiter inside the identifier. Note: unlike a '-literal, an identifier's
			// bytes DO survive (lower-cased) — a table/column name is structure, not the
			// volatile value the redaction promise covers.
			q := c
			i++
			var ident strings.Builder
			for i < n {
				if raw[i] == q {
					if i+1 < n && raw[i+1] == q {
						ident.WriteByte(q)
						i += 2
						continue
					}
					i++
					break
				}
				ident.WriteByte(raw[i])
				i++
			}
			toks = append(toks, emitIdent(ident.String()))
		case c == '$':
			// PostgreSQL dollar-quoted string literal ($tag$ … $tag$): consume the
			// whole body so NO byte inside it survives into the canonical statement,
			// the same redaction promise this file makes for '-literals (H-1). A bare
			// $N bind placeholder ($1) is NOT a dollar quote and falls through to the
			// placeholder run below.
			if d := dollarQuoteDelim(raw, i); d > 0 {
				delim := raw[i : i+d]
				if k := strings.Index(raw[i+d:], delim); k >= 0 {
					// A well-formed dollar-quoted literal (matching close found): redact
					// its whole body REGARDLESS of what precedes it, honoring H-1 even
					// when the literal abuts an identifier byte (x$$secret$$). Requiring a
					// matching close is what lets an identifier's embedded '$' (a$b$c,
					// which has no close) fall through below without swallowing the tail.
					i += d + k + d // consume body AND the closing delimiter
					toks = append(toks, "?")
					continue
				}
				// No matching close. At a token boundary this is an UNTERMINATED literal
				// — consume the rest (fail-closed redaction, never leak). Glued to an
				// identifier byte it is instead an identifier's embedded '$' (a$b$c):
				// fall through so the tail is NOT swallowed (Table would drift to "").
				// This token-boundary guard is the parity partner of
				// schemadrift.stripSQL's isIdentByte(s[i-1]) check; the two stay in step.
				if i == 0 || !isIdentByte(raw[i-1]) {
					i = n
					toks = append(toks, "?")
					continue
				}
			}
			// Driver placeholder ($1) or an identifier-continuation '$': consume the run.
			i++
			for i < n && (isIdent(raw[i]) || isDigit(raw[i])) {
				i++
			}
			toks = append(toks, "?")
		case c == ':' || c == '@':
			// Driver placeholder (:name, @p1): consume the run.
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
			// Emit the raw byte verbatim. string([]byte{c}) preserves c exactly;
			// string(c) would interpret the byte as a rune and re-encode a non-ASCII
			// byte (>=0x80, e.g. a UTF-8 identifier outside quotes) into a different,
			// longer byte sequence — corrupting the statement and breaking the
			// idempotence the canonical form depends on (FuzzNormalizeIdempotent).
			toks = append(toks, string([]byte{c}))
			i++
		}
	}
	return toks
}

var (
	// inClause and valuesClause collapse only the placeholder lists whose
	// cardinality is legitimately variable: IN (?, ?, …) and one-or-more VALUES
	// rows (?, …), (?, …). Both are ANCHORED to their keyword so a SQL function
	// call's argument list (coalesce(?, ?)) is left intact — collapsing that would
	// merge structurally distinct statements (coalesce(?, ?) vs coalesce(?, ?, ?))
	// under one canonical key and report "no change" where the SQL shape changed.
	inClause     = regexp.MustCompile(`\bIN\s*\(\s*\?(\s*,\s*\?)*\s*\)`)
	valuesClause = regexp.MustCompile(`\bVALUES\s*\(\s*\?(\s*,\s*\?)*\s*\)(\s*,\s*\(\s*\?(\s*,\s*\?)*\s*\))*`)
	wsRun        = regexp.MustCompile(`\s+`)
)

// assemble joins tokens with single spaces and collapses the variable-cardinality
// placeholder lists — IN (?, ?, ?) → IN (?) and multi-row VALUES (?), (?) →
// VALUES (?) — so a statement's value count does not perturb the golden. A
// function-call argument list is NOT a variable-cardinality clause and is left
// untouched (see inClause/valuesClause).
func assemble(toks []string) string {
	out := strings.Join(toks, " ")
	out = inClause.ReplaceAllString(out, "IN (?)")
	out = valuesClause.ReplaceAllString(out, "VALUES (?)")
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
	case "INSERT", "DELETE", "MERGE", "REPLACE", "UPSERT":
		// MERGE/REPLACE/UPSERT target their table after INTO, like INSERT; DELETE
		// after FROM. identAfter accepts either so the verb set stays in one case.
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

// tableIdent returns the table-name value a table-position token carries, and
// whether the token is a table identifier at all. It accepts a bare lower-case
// identifier verbatim AND a re-quoted identifier — emitIdent's output for a
// keyword-spelled or otherwise non-plain name (e.g. `"order"`, `"user"`) — which it
// unwraps back to the bare name. Without this, a quoted reserved-word table would
// lose its Table attribution (the token starts with '"', failing isIdent), merging
// distinct tables under one op key. Returns ok=false for a placeholder ("?"),
// punctuation, or an upper-cased keyword token.
func tableIdent(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	if tok[0] == '"' {
		return unquoteIdent(tok), true
	}
	if isIdent(tok[0]) && tok == strings.ToLower(tok) {
		return tok, true
	}
	return "", false
}

// unquoteIdent reverses emitIdent's re-quoting: it strips the wrapping double
// quotes and un-doubles any embedded "" back to a single ". The bytes were already
// lower-cased by emitIdent, so the result matches the bare-identifier form.
func unquoteIdent(tok string) string {
	if len(tok) < 2 || tok[0] != '"' || tok[len(tok)-1] != '"' {
		return tok
	}
	return strings.ReplaceAll(tok[1:len(tok)-1], `""`, `"`)
}

// updateTable returns the target table of an UPDATE, skipping any leading
// qualifier words so "UPDATE ONLY loans SET ..." yields "loans", not "only".
func updateTable(toks []string) string {
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		if updateQualifiers[t] {
			continue
		}
		if tbl, ok := tableIdent(t); ok {
			return tbl
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
			if tbl, ok := tableIdent(toks[i+1]); ok {
				return tbl
			}
		}
	}
	return ""
}

// dollarQuoteDelim returns the byte length of a PostgreSQL dollar-quote opening
// delimiter beginning at raw[i] ("$$" → 2, "$body$" → 6), or 0 if raw[i] does not
// open one. The tag between the two '$' follows the unquoted-identifier rule
// (letters, digits, underscores; never starting with a digit) and may be empty,
// so a "$1" bind placeholder — a digit right after the first '$' — is correctly
// NOT a delimiter. Parallels schemadrift.dollarQuoteDelim (same grammar).
func dollarQuoteDelim(raw string, i int) int {
	if i >= len(raw) || raw[i] != '$' {
		return 0
	}
	for j := i + 1; j < len(raw); j++ {
		c := raw[j]
		switch {
		case c == '$':
			return j - i + 1
		case isIdent(c):
			// tag byte (isIdent covers letters and '_')
		case isDigit(c):
			if j == i+1 {
				return 0 // a tag cannot start with a digit ($1 is a placeholder)
			}
		default:
			return 0
		}
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdent(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isIdentByte reports whether c can appear *inside* an identifier. Unlike isIdent
// it also admits digits and '$' (PostgreSQL allows '$' inside identifiers, never
// as the first character), so a '$' following one of these is an identifier
// continuation, not a dollar-quote opener. Kept byte-for-byte in step with
// schemadrift.isIdentByte (the dollar-quote token-boundary guard on both sides).
func isIdentByte(c byte) bool {
	return c == '$' || isIdent(c) || isDigit(c)
}

// emitIdent turns an unwrapped quoted-identifier's bytes into a token that
// re-normalizes to ITSELF — the fixed-point property FuzzNormalizeIdempotent
// enforces and the canonical key depends on. When the lower-cased bytes form a
// plain identifier that is not a keyword, we emit them bare so a quoted name keys
// identically to the same name written unquoted (M-24/M-25). Otherwise a bare
// emit would re-tokenize DIFFERENTLY on the next pass — digits-leading bytes
// become a number "?", an embedded ' opens a literal, an embedded -- or /* opens
// a comment that eats the tail, a keyword-spelled name re-upper-cases — which
// both breaks idempotence AND re-opens the M-24 leak class (the quoted body
// spills out reinterpreted). Re-quoting (doubling any embedded quote) always
// round-trips: the second pass unwraps it right back to these same bytes.
func emitIdent(raw string) string {
	low := strings.ToLower(raw)
	if isBareIdent(low) {
		return low
	}
	var b strings.Builder
	b.Grow(len(low) + 2)
	b.WriteByte('"')
	for i := 0; i < len(low); i++ {
		if low[i] == '"' {
			b.WriteByte('"') // double an embedded quote so the re-quoted form re-parses
		}
		b.WriteByte(low[i])
	}
	b.WriteByte('"')
	return b.String()
}

// isBareIdent reports whether s (already lower-cased) can be emitted as an
// unquoted identifier token that re-tokenizes to the identical single token: it
// must be non-empty, start with an identifier byte (not a digit), contain only
// identifier/digit bytes, and not be a keyword (which would re-upper-case).
func isBareIdent(s string) bool {
	if s == "" || !isIdent(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdent(s[i]) && !isDigit(s[i]) {
			return false
		}
	}
	return !keywords[s]
}
