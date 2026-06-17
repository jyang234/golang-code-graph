// Package impeach implements the static × behavioral impeachment cell — a
// counterexample finder for the static analyzer's own negatives
// (docs/design/behavioral-impeachment-plan.md). It joins a stamped static graph
// against a canonical behavioral trace corpus, keyed on the (emitting-site,
// effect-label) pair, so a behaviorally-observed effect on a statically-NEVER
// path surfaces as a witness that the over-approximation's blind-spot disclosure
// was incomplete.
//
// This file holds the join's most load-bearing primitive: the ONE place the two
// sides' spellings of a database effect are reconciled to a single key. Get this
// parity wrong and the join either misses real impeachments (a false-negative
// audit) or invents spurious ones — both failures the parity test guards against.
package impeach

import "strings"

// DBEffectKey is the canonical, system-agnostic join key for a database boundary
// effect: "db <OP> <table>". It is the single point where the STATIC boundary
// label (graph.DBEffect, decoded from "boundary:db DELETE ledger") and the
// BEHAVIORAL op key (opkey.ParseDBKey, decoded from "DB postgresql DELETE
// ledger") are reduced to one string, so the observed×unreachable join compares
// like with like (plan §14-A; the ladder's `label` rung, §4).
//
// The DB *system* is deliberately dropped. The static side cannot know it — SSA
// does not carry db.system — so a key that included the system could never match
// a static negative, and EVERY database write would read as unmatched (a join
// that silently sees nothing: the prime directive's worst failure). Keying on
// only what static can express keeps the join sound; the behavioral system
// qualifier rides into the witness Observation as enrichment, never into the key.
//
// op is upper-cased so the static label's verb (DELETE) and the behavioral op
// key's verb (already upper-cased by dbKey) cannot diverge on case alone. table
// is kept verbatim — table identity is the consumer's, not ours to fold.
func DBEffectKey(op, table string) string {
	return "db " + strings.ToUpper(op) + " " + table
}
