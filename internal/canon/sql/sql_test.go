package sql

import "testing"

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

func TestNormalizeDeterministic(t *testing.T) {
	// Whitespace and placeholder-style variations of the same logical statement
	// converge.
	a := Normalize("SELECT  id ,\n status\tFROM loans WHERE id = $1").Statement
	b := Normalize("SELECT id, status FROM loans WHERE id = ?").Statement
	if a != b {
		t.Errorf("whitespace/placeholder variants diverged: %q != %q", a, b)
	}
}
