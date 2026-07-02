package canon

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestDBOperationParity pins that the canon effect classifier and the op-key
// builder derive a DB span's operation verb through the ONE shared function
// (opkey.DBOperation) — the precedence (db.operation → db.operation.name →
// statement-normalized) lived in two hand-rolled copies before M-10. A future edit
// that re-inlines a divergent copy in either place would split the behavioral-
// impeachment join (the op key) from the effect classification (canon), so this
// test locks the two together: canon.dbOperation must equal opkey.DBOperation, and
// the operation embedded in the assembled DB op key must be the same verb.
func TestDBOperationParity(t *testing.T) {
	cases := []map[string]string{
		{"db.system": "postgresql", "db.operation": "select"},
		{"db.system": "postgresql", "db.operation.name": "Insert"},
		{"db.system": "postgresql", "db.statement": "UPDATE applicants SET x = 1 WHERE id = $1"},
		{"db.system": "postgresql", "db.statement": "delete from ledger where id = 1"},
		// attribute wins over the statement when both are present.
		{"db.system": "postgresql", "db.operation": "select", "db.statement": "INSERT INTO x VALUES (1)"},
		// opaque statement, no operation attribute → empty verb (never guessed).
		{"db.system": "postgresql", "db.statement": "CALL do_thing($1)"},
	}

	for _, attrs := range cases {
		canonOp := dbOperation(attrs)
		sharedOp := opkey.DBOperation(attrs)
		if canonOp != sharedOp {
			t.Errorf("attrs %v: canon.dbOperation=%q != opkey.DBOperation=%q", attrs, canonOp, sharedOp)
		}

		// The verb embedded in the DB op key must be the same one.
		key, _ := opkey.Of(ir.KindClient, attrs, "")
		_, keyOp, _, ok := opkey.ParseDBKey(key)
		if !ok {
			t.Errorf("attrs %v: op key %q is not a DB key", attrs, key)
			continue
		}
		if keyOp != sharedOp {
			t.Errorf("attrs %v: op-key operation %q != shared derivation %q — the two DB-verb derivations diverged", attrs, keyOp, sharedOp)
		}
	}
}
