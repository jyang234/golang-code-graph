package sql

import (
	"strings"
	"testing"
)

func TestNormalizeStripsLiterals(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"SELECT name, income FROM applicants WHERE id = $1", "SELECT name , income FROM applicants WHERE id = ?"},
		{"SELECT name FROM applicants WHERE id = 8412", "SELECT name FROM applicants WHERE id = ?"},
		{"select * from loans where status = 'paid'", "SELECT * FROM loans WHERE status = ?"},
		{"INSERT INTO ledger (loan_id, amount) VALUES ($1, $2)", "INSERT INTO ledger ( loan_id , amount ) VALUES (?)"},
	}
	for _, c := range cases {
		if got := Normalize(c.raw).Statement; got != c.want {
			t.Errorf("Normalize(%q).Statement = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestNormalizeCollapsesInList(t *testing.T) {
	got := Normalize("SELECT * FROM loans WHERE id IN (1, 2, 3, 4, 5)").Statement
	want := "SELECT * FROM loans WHERE id IN (?)"
	if got != want {
		t.Errorf("IN-list: got %q, want %q", got, want)
	}
}

func TestNormalizeCollapsesMultiRow(t *testing.T) {
	got := Normalize("INSERT INTO t (a, b) VALUES (1, 2), (3, 4), (5, 6)").Statement
	want := "INSERT INTO t ( a , b ) VALUES (?)"
	if got != want {
		t.Errorf("multi-row: got %q, want %q", got, want)
	}
}

func TestNormalizeStableUnderCardinality(t *testing.T) {
	// 3 rows vs 300 rows must normalize identically — the whole point.
	three := Normalize("INSERT INTO t (a) VALUES (1), (2), (3)").Statement
	many := Normalize("INSERT INTO t (a) VALUES (1), (2), (3), (4), (5), (6), (7)").Statement
	if three != many {
		t.Errorf("cardinality leaked: %q != %q", three, many)
	}
}

func TestOperationAndTable(t *testing.T) {
	cases := []struct {
		raw, op, table string
	}{
		{"SELECT name FROM applicants WHERE id = $1", "SELECT", "applicants"},
		{"INSERT INTO ledger (loan_id) VALUES ($1)", "INSERT", "ledger"},
		{"UPDATE loans SET status = 'paid' WHERE id = $1", "UPDATE", "loans"},
		{"DELETE FROM sessions WHERE id = $1", "DELETE", "sessions"},
	}
	for _, c := range cases {
		n := Normalize(c.raw)
		if n.Operation != c.op || n.Table != c.table {
			t.Errorf("Normalize(%q) = {op:%q table:%q}, want {op:%q table:%q}", c.raw, n.Operation, n.Table, c.op, c.table)
		}
	}
}

// TestOperationAndTableUpdateQualifier covers UPDATE statements with a leading
// dialect qualifier (Postgres ONLY, MySQL LOW_PRIORITY/IGNORE): the table must
// be the real target, not the qualifier word.
func TestOperationAndTableUpdateQualifier(t *testing.T) {
	cases := []struct {
		raw, table string
	}{
		{"UPDATE ONLY loans SET status = 'paid' WHERE id = $1", "loans"},
		{"UPDATE LOW_PRIORITY accounts SET balance = 0", "accounts"},
		{"UPDATE loans SET status = 'paid'", "loans"},
	}
	for _, c := range cases {
		n := Normalize(c.raw)
		if n.Operation != "UPDATE" || n.Table != c.table {
			t.Errorf("Normalize(%q) = {op:%q table:%q}, want UPDATE/%q", c.raw, n.Operation, n.Table, c.table)
		}
	}
}

// TestNormalizeKeepsFunctionCallArity pins that the placeholder-list collapse is
// scoped to variable-cardinality clauses (IN-lists, VALUES rows) and does NOT
// collapse a SQL function call's argument list. coalesce(?, ?) and
// coalesce(?, ?, ?) are structurally distinct statements; collapsing both to
// coalesce (?) would merge them under one canonical key and report "no change"
// where the SQL shape actually changed.
func TestNormalizeKeepsFunctionCallArity(t *testing.T) {
	two := Normalize("SELECT coalesce(?, ?) FROM t").Statement
	three := Normalize("SELECT coalesce(?, ?, ?) FROM t").Statement
	if two == three {
		t.Errorf("function-call arity collapsed: %q == %q (distinct statements merged)", two, three)
	}
	if !strings.Contains(two, "? , ?") {
		t.Errorf("function-call placeholders were collapsed: %q", two)
	}
	// The IN-list and VALUES collapses must still hold (their cardinality IS
	// variable and legitimately churns the golden otherwise).
	if got := Normalize("SELECT * FROM t WHERE id IN (1, 2, 3)").Statement; got != "SELECT * FROM t WHERE id IN (?)" {
		t.Errorf("IN-list no longer collapses: %q", got)
	}
	if got := Normalize("INSERT INTO t (a, b) VALUES (1, 2), (3, 4)").Statement; got != "INSERT INTO t ( a , b ) VALUES (?)" {
		t.Errorf("multi-row VALUES no longer collapses: %q", got)
	}
}

// TestOperationAndTableMergeReplaceUpsert pins that the verbs sqlverb.Mutating
// treats as writes (MERGE/UPSERT/REPLACE) are parsed with their operation
// upper-cased and their target table extracted, identically to INSERT — so the
// canonical op key for a MERGE matches the static labeler's and the table is not
// dropped.
func TestOperationAndTableMergeReplaceUpsert(t *testing.T) {
	cases := []struct {
		raw, op, table string
	}{
		{"MERGE INTO accounts USING staging ON accounts.id = staging.id", "MERGE", "accounts"},
		{"REPLACE INTO sessions (id, data) VALUES ($1, $2)", "REPLACE", "sessions"},
		{"UPSERT INTO counters (k, n) VALUES ($1, $2)", "UPSERT", "counters"},
	}
	for _, c := range cases {
		n := Normalize(c.raw)
		if n.Operation != c.op || n.Table != c.table {
			t.Errorf("Normalize(%q) = {op:%q table:%q}, want {op:%q table:%q}", c.raw, n.Operation, n.Table, c.op, c.table)
		}
	}
}

// TestNormalizeBackslashEscapedQuote pins H-1: a backslash-escaped quote inside
// a literal (MySQL/MariaDB default) must not terminate the literal early and
// spill captured user data out as identifier tokens.
func TestNormalizeBackslashEscapedQuote(t *testing.T) {
	got := Normalize(`INSERT INTO t (a,b) VALUES ('O\'Brien', 'secret-value-42')`).Statement
	want := "INSERT INTO t ( a , b ) VALUES (?)"
	if got != want {
		t.Errorf("backslash-escaped quote leaked: got %q, want %q", got, want)
	}
	// "value" is deliberately excluded: it is a substring of the VALUES keyword,
	// not a leak. These fragments cannot appear in the collapsed skeleton.
	for _, leaked := range []string{"brien", "secret"} {
		if strings.Contains(strings.ToLower(got), leaked) {
			t.Errorf("literal fragment %q survived into %q", leaked, got)
		}
	}
}

// TestNormalizeStripsComments pins M-24: line and block comments (which carry
// per-request volatile payloads like sqlcommenter tags) are dropped, never
// tokenized into the canonical statement.
func TestNormalizeStripsComments(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"SELECT * FROM t -- secret=comment-42\nWHERE id = 1", "SELECT * FROM t WHERE id = ?"},
		{"SELECT /* route='/api/x' */ a FROM t", "SELECT a FROM t"},
		{"SELECT 1 /* trailing */", "SELECT ?"},
		{"SELECT 1 -- dangling comment with no newline", "SELECT ?"},
	}
	for _, c := range cases {
		if got := Normalize(c.raw).Statement; got != c.want {
			t.Errorf("Normalize(%q).Statement = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestNormalizeQuotedIdentifier pins M-24/M-25: a double-quoted or backtick
// identifier is unwrapped and lower-cased so it keys identically to the bare
// form and Table extraction still finds it (rather than collapsing to "?").
func TestNormalizeQuotedIdentifier(t *testing.T) {
	cases := []struct {
		raw, stmt, table string
	}{
		{`SELECT * FROM "Applicants" WHERE id = 1`, "SELECT * FROM applicants WHERE id = ?", "applicants"},
		{"SELECT * FROM `Orders`", "SELECT * FROM orders", "orders"},
		{`UPDATE "Loans" SET status = 'paid'`, "UPDATE loans SET status = ?", "loans"},
	}
	for _, c := range cases {
		n := Normalize(c.raw)
		if n.Statement != c.stmt {
			t.Errorf("Normalize(%q).Statement = %q, want %q", c.raw, n.Statement, c.stmt)
		}
		if n.Table != c.table {
			t.Errorf("Normalize(%q).Table = %q, want %q", c.raw, n.Table, c.table)
		}
	}
}

// TestNormalizeStripsDollarQuotedLiteral pins the review fix: a PostgreSQL
// dollar-quoted string literal ($tag$ … $tag$) is a quoted literal, so no byte of
// its body may survive into the canonical statement — the same redaction promise
// the '-literal path makes. A bare $1 bind placeholder is not a dollar quote.
func TestNormalizeStripsDollarQuotedLiteral(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"SELECT $$secret token value$$", "SELECT ?"},
		{"SELECT $tag$O'Brien-secret$tag$ FROM t", "SELECT ? FROM t"},
		{"INSERT INTO t (a) VALUES ($$multi\nline secret$$)", "INSERT INTO t ( a ) VALUES (?)"},
		// A $N placeholder is NOT a dollar quote and must stay a single "?".
		{"SELECT a FROM t WHERE id = $1", "SELECT a FROM t WHERE id = ?"},
	}
	for _, c := range cases {
		got := Normalize(c.raw).Statement
		if got != c.want {
			t.Errorf("Normalize(%q).Statement = %q, want %q", c.raw, got, c.want)
		}
		for _, leaked := range []string{"secret", "brien", "token"} {
			if strings.Contains(strings.ToLower(got), leaked) {
				t.Errorf("dollar-quoted body fragment %q survived into %q", leaked, got)
			}
		}
	}
}

// TestNormalizeQuotedIdentifierIdempotent pins R-2: an unwrapped quoted
// identifier whose bytes are not a plain non-keyword identifier must NOT be
// emitted bare (it would re-tokenize differently on a second pass and could spill
// its bytes back out as a literal/comment). Re-quoting makes the canonical form a
// fixed point of its own normalizer.
func TestNormalizeQuotedIdentifierIdempotent(t *testing.T) {
	cases := []string{
		`SELECT "0000000000000000" FROM t`, // digit-only ident: bare emit would become "?"
		`SELECT * FROM "from"`,             // keyword-spelled ident: bare emit re-upper-cases
		`SELECT "a'b" FROM t`,              // embedded ' would open a literal on re-parse
		`SELECT "a--b" FROM t`,             // embedded -- would open a comment on re-parse
		`SELECT "a/*b" FROM t`,             // embedded /* would open a block comment
		"SELECT `back``tick` FROM t",       // embedded (doubled) delimiter
	}
	for _, raw := range cases {
		once := Normalize(raw).Statement
		twice := Normalize(once).Statement
		if once != twice {
			t.Errorf("Normalize(%q) not idempotent:\n once: %q\n twice: %q", raw, once, twice)
		}
	}
}

// TestNormalizeNestedBlockComment pins R-2: PostgreSQL nests /* */, so a
// depth-blind scan would stop at the first */ and spill the outer comment's tail
// (which may carry volatile/PII payloads) into the canonical statement.
func TestNormalizeNestedBlockComment(t *testing.T) {
	got := Normalize("SELECT 1 /* outer /* inner */ secret-payload-99 */ , 2 FROM t").Statement
	const want = "SELECT ? , ? FROM t"
	if got != want {
		t.Errorf("nested block comment mishandled: got %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(got), "secret") || strings.Contains(got, "payload") {
		t.Errorf("nested-comment body leaked into canonical statement: %q", got)
	}
}

// TestNormalizeDollarInIdentifier pins R-2: a '$' that continues an identifier
// (prev byte is an identifier byte) is not a dollar-quote opener; the guard
// parallels schemadrift.stripSQL's isIdentByte(s[i-1]). Without it, "a$b$c"
// reads "$b$" as a dollar quote and swallows the FROM clause, dropping the table.
func TestNormalizeDollarInIdentifier(t *testing.T) {
	n := Normalize("SELECT a$b$c FROM t")
	if n.Table != "t" {
		t.Errorf("'$'-in-identifier swallowed the FROM clause: Table = %q, want %q", n.Table, "t")
	}
	if once, twice := n.Statement, Normalize(n.Statement).Statement; once != twice {
		t.Errorf("'$'-in-identifier not idempotent: once %q, twice %q", once, twice)
	}
}

// TestNormalizeQuotedKeywordTable pins the R-2 follow-up: a table whose name is a
// SQL keyword must be quoted, and re-quoting it for idempotence must NOT lose the
// Table attribution (which collapses distinct tables under one op key). Table
// extraction unwraps the re-quoted token back to the bare name.
func TestNormalizeQuotedKeywordTable(t *testing.T) {
	cases := []struct {
		raw, table string
	}{
		{`INSERT INTO "order" (id) VALUES (1)`, "order"},
		{`SELECT * FROM "select"`, "select"},
		{`DELETE FROM "group" WHERE id = 1`, "group"},
		{`UPDATE "limit" SET x = 1`, "limit"},
		{`INSERT INTO "Order" (id) VALUES (1)`, "order"}, // case-folded like bare
	}
	for _, c := range cases {
		n := Normalize(c.raw)
		if n.Table != c.table {
			t.Errorf("Normalize(%q).Table = %q, want %q", c.raw, n.Table, c.table)
		}
		// And still a fixed point of the normalizer.
		if once, twice := n.Statement, Normalize(n.Statement).Statement; once != twice {
			t.Errorf("Normalize(%q) not idempotent: once %q twice %q", c.raw, once, twice)
		}
	}
}

// TestNormalizeDollarLiteralAbuttingIdentRedacted pins the R-2 follow-up: a
// well-formed dollar-quoted literal must be redacted even when it directly abuts an
// identifier byte (no separating space), since the token-boundary guard alone would
// have let its body leak (H-1). The discriminator is a matching close delimiter.
func TestNormalizeDollarLiteralAbuttingIdentRedacted(t *testing.T) {
	for _, raw := range []string{
		`SELECT x AS$$ secret payload $$`, // $$ glued to the AS keyword's tail
		`SELECT a$$secret$$`,              // glued to an identifier
	} {
		got := Normalize(raw).Statement
		for _, leaked := range []string{"secret", "payload"} {
			if strings.Contains(strings.ToLower(got), leaked) {
				t.Errorf("dollar-literal body %q leaked into %q (from %q)", leaked, got, raw)
			}
		}
	}
}

// TestIsIdentByteGrammar pins the PostgreSQL identifier-continuation byte set this
// scanner's dollar-quote token-boundary guard depends on. schemadrift.isIdentByte
// must match this byte-for-byte (the "kept in step" parity claim); pinning both
// copies to the SAME explicit spec means a silent edit to either now fails its own
// package's test — the guard CLAUDE.md requires for a rule applied in two places.
// (schemadrift/parser_test.go carries the identical assertion.)
func TestIsIdentByteGrammar(t *testing.T) {
	for c := 0; c < 256; c++ {
		b := byte(c)
		want := b == '_' || b == '$' ||
			(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
		if isIdentByte(b) != want {
			t.Errorf("isIdentByte(%q) = %v, want %v", b, isIdentByte(b), want)
		}
	}
}

func TestNormalizeDeterministic(t *testing.T) {
	// Whitespace and placeholder-style variations of the same logical statement
	// converge.
	a := Normalize("SELECT  id ,\n status\tFROM loans WHERE id = $1").Statement
	b := Normalize("SELECT id, status FROM loans WHERE id = ?").Statement
	if a != b {
		t.Errorf("whitespace/placeholder variants diverged: %q != %q", a, b)
	}
}
