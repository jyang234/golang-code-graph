package sqlverb

import "testing"

func TestMutating(t *testing.T) {
	for _, op := range []string{"INSERT", "UPDATE", "DELETE", "UPSERT", "MERGE", "REPLACE"} {
		if !Mutating(op) {
			t.Errorf("Mutating(%q) = false, want true", op)
		}
	}
	// Reads, transaction control, the unreadable fallback, and lower-case input
	// must NOT be treated as committed writes.
	for _, op := range []string{"SELECT", "BEGIN", "COMMIT", "EXEC", "insert", ""} {
		if Mutating(op) {
			t.Errorf("Mutating(%q) = true, want false", op)
		}
	}
}
