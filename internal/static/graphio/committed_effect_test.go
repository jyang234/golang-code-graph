package graphio

import "testing"

// committedEffect feeds the partial-effect / effect_order disclosure: a fallible
// call after a committed write is the inconsistent-state case it exists to
// surface. The DB-mutation verb set must match fitness.IsWrite
// (INSERT/UPDATE/DELETE/UPSERT/MERGE/REPLACE) so a constant MERGE/UPSERT/REPLACE
// statement is not silently dropped from the disclosure.
func TestCommittedEffect(t *testing.T) {
	committed := []string{
		"boundary:bus PUBLISH orders",
		"boundary:db INSERT users",
		"boundary:db UPDATE loans",
		"boundary:db DELETE sessions",
		"boundary:db UPSERT inventory",
		"boundary:db MERGE ledger",
		"boundary:db REPLACE cache",
	}
	for _, l := range committed {
		if !committedEffect(l) {
			t.Errorf("committedEffect(%q) = false, want true (committed write dropped from disclosure)", l)
		}
	}

	notCommitted := []string{
		"boundary:db SELECT users",       // read
		"boundary:db Exec",               // dynamic SQL the labeler could not read — handled by the unclassified channel, not asserted here
		"boundary:http GET peer",         // outbound read
		"boundary:db call",               // opaque fallback
		"boundary:bus PUBLISH <dynamic>", // non-constant event — disclosed via the dynamic channel, not asserted as a named publish
	}
	for _, l := range notCommitted {
		if committedEffect(l) {
			t.Errorf("committedEffect(%q) = true, want false", l)
		}
	}
}
