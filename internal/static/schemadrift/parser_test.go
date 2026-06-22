package schemadrift

import (
	"sort"
	"testing"
)

func ddlNames(ops []ddlOp, create bool) []string {
	var out []string
	for _, o := range ops {
		if o.create == create {
			out = append(out, o.name)
		}
	}
	sort.Strings(out)
	return out
}

func eqSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A CREATE/DROP inside a comment or a string literal must NOT be scanned as real DDL
// — a phantom CREATE would mask real drift (the unsound direction), a phantom DROP
// would manufacture it.
func TestScanDDLIgnoresCommentsAndLiterals(t *testing.T) {
	sqlText := `
-- CREATE TABLE commented_out (id int);
/* CREATE TABLE block_commented (id int); DROP TABLE real_one; */
CREATE TABLE real_one (id int);
INSERT INTO audit(note) VALUES ('CREATE TABLE shadow; DROP TABLE real_one');
`
	ops := scanDDL(sqlText)
	if got := ddlNames(ops, true); !eqSet(got, []string{"real_one"}) {
		t.Errorf("creates = %v, want [real_one] (comment/literal CREATEs must be ignored)", got)
	}
	if got := ddlNames(ops, false); len(got) != 0 {
		t.Errorf("drops = %v, want none (the DROP lives only in a comment and a literal)", got)
	}
}

// A DROP without a trailing semicolon must not swallow the following statement's
// column list into the drop set.
func TestScanDDLDropWithoutSemicolonDoesNotBleed(t *testing.T) {
	// Note: statements ARE semicolon-terminated here; the risk is the LAST drop with
	// no terminator. Put the unterminated DROP last.
	sqlText := "CREATE TABLE keep (a int);\nDROP TABLE gone"
	ops := scanDDL(sqlText)
	if got := ddlNames(ops, true); !eqSet(got, []string{"keep"}) {
		t.Errorf("creates = %v, want [keep]", got)
	}
	if got := ddlNames(ops, false); !eqSet(got, []string{"gone"}) {
		t.Errorf("drops = %v, want [gone] (must not capture column lists or other tokens)", got)
	}
}

// CREATE UNLOGGED / TEMPORARY TABLE (real persistent / session tables) must be
// recognized, or they'd be missing from the defined set and manufacture false drift.
func TestScanDDLRecognizesTableQualifiers(t *testing.T) {
	sqlText := `
CREATE UNLOGGED TABLE u (id int);
CREATE TEMPORARY TABLE t (id int);
CREATE TABLE plain AS SELECT 1;
`
	if got := ddlNames(scanDDL(sqlText), true); !eqSet(got, []string{"plain", "t", "u"}) {
		t.Errorf("creates = %v, want [plain t u]", got)
	}
}

// isRollback matches the *_rollback.sql suffix, NOT a loose substring — a forward
// migration whose description merely contains "rollback" must replay.
func TestIsRollbackSuffixOnly(t *testing.T) {
	if !isRollback("V8__drop_queue_rollback.sql") {
		t.Error("a *_rollback.sql file must be excluded from forward replay")
	}
	if isRollback("V12__add_rollback_audit_table.sql") {
		t.Error("a forward migration whose name merely contains 'rollback' must NOT be excluded")
	}
}

// End-to-end: a commented-out CREATE must not become a phantom table that masks a
// real drift on that table name.
func TestCommentedCreateDoesNotMaskDrift(t *testing.T) {
	files := []MigrationFile{mig("V1__init.sql", "-- CREATE TABLE ghost (id int);\nCREATE TABLE real (id int);")}
	edges := []Edge{dbEdge("svc.W", "INSERT ghost")}
	r := Check(edges, files, nil)
	if len(r.Drift) != 1 || r.Drift[0].Table != "ghost" {
		t.Fatalf("a write to a table only 'defined' in a comment must drift; got %v", r.Drift)
	}
}
