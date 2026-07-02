package sql

import (
	"strings"
	"testing"
)

// FuzzNormalizeNoLiteralLeak is the leak-oriented property test H-1 calls for:
// NO byte sequence from inside a single-quoted literal may survive into
// Normalized.Statement. It embeds an arbitrary fuzzer-chosen secret inside a
// well-formed literal (escaping the two bytes that could end it — \ and ') and
// asserts the normal form collapses to the fixed skeleton with the literal
// reduced to a single "?". This is stronger than idempotence: idempotence is
// blind to a literal that leaks its content the very first pass (the exact H-1
// backslash-escape bug), whereas this fails the moment any secret byte escapes.
func FuzzNormalizeNoLiteralLeak(f *testing.F) {
	for _, s := range []string{
		"secret-value-42",
		"O'Brien",
		`back\slash`,
		"'; DROP TABLE users; --",
		"multi\nline\tsecret",
		"",
		`\`,
		`''`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, secret string) {
		// Make `secret` a genuine, well-formed literal body: escape backslashes
		// first (so the \ we add before quotes is not itself re-escaped), then the
		// single quotes. The only unescaped ' left is our closing delimiter.
		escaped := strings.ReplaceAll(secret, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `'`, `\'`)
		stmt := "SELECT * FROM t WHERE note = '" + escaped + "'"
		const want = "SELECT * FROM t WHERE note = ?"
		if got := Normalize(stmt).Statement; got != want {
			t.Errorf("literal content leaked into canonical statement:\n secret: %q\n got:    %q\n want:   %q", secret, got, want)
		}
	})
}

// FuzzNormalizeIdempotent makes the SQL normalizer self-checking on two
// properties that both review rounds' findings violated in spirit:
//   - it never panics on arbitrary statement text (the gated db.statement is
//     attacker-influenced), and
//   - the normal form is a FIXED POINT: re-normalizing an already-normalized
//     statement yields the identical statement. Idempotence is what guarantees
//     a statement's canonical key is stable no matter how many normalization
//     passes it survives, and it catches a collapse rule (IN/VALUES/function
//     args) that is not self-consistent.
func FuzzNormalizeIdempotent(f *testing.F) {
	for _, s := range []string{
		"SELECT name, income FROM applicants WHERE id = $1",
		"select * from loans where id IN (1,2,3)",
		"INSERT INTO t (a,b) VALUES ($1,$2),($3,$4)",
		"MERGE INTO accounts USING staging ON accounts.id = staging.id",
		"SELECT coalesce(?, ?, ?) FROM t",
		"UPDATE ONLY loans SET status = 'paid'",
		"",
		")((((",
		"'unterminated",
		"0\x850",      // regression: a non-ASCII byte outside quotes (FuzzNormalizeIdempotent found this)
		"SELECT café", // UTF-8 identifier outside quotes must round-trip, not grow
		// R-2 regressions: an unwrapped quoted identifier must not re-tokenize into
		// something else on the second pass (a number, a keyword, a literal/comment).
		`"0000000000000000"`,          // digit-only quoted ident (was: → 0000… → ?)
		`"from"`,                      // keyword-spelled quoted ident (was re-upper-cased)
		`SELECT "a'b" FROM "c--d"`,    // embedded ' and -- inside quoted idents
		`SELECT "a/*b" FROM t`,        // embedded /* inside a quoted ident
		"/* outer /* inner */ x */ 1", // nested block comment must not spill "x"
		"SELECT a$b$c FROM t",         // '$' continuing an identifier, not a dollar quote
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		n := Normalize(raw)
		again := Normalize(n.Statement)
		if again.Statement != n.Statement {
			t.Errorf("Normalize not idempotent:\n in:   %q\n once: %q\n twice:%q", raw, n.Statement, again.Statement)
		}
	})
}
