// Package sqlverb is the single source of truth for which SQL verbs commit a row
// mutation. The static labeler (graphio.mutatingSQLOp) and the groundwork write
// surface (fitness.IsWrite / fitness.UnclassifiedDBLabel) are otherwise
// decoupled — they communicate only through the emitted graph JSON — so the
// mutating-verb set lives here, imported by both, instead of being copied into
// each and left to drift. The partial-effect disclosure and the I/O-budget write
// surface must agree on what counts as a committed write; this package is what
// makes that parity mechanical rather than a hand-maintained comment.
package sqlverb

// Mutating reports whether an uppercase SQL verb commits a row mutation. Callers
// pass an already upper-cased verb — the form graphio reads off a constant
// statement and the form fitness derives from the boundary label. A verb the
// labeler could NOT read (a dynamic statement that fell back to the driver method
// name) is not in this set and must be routed through the unclassified-DB caution
// channel rather than asserted as a definite write.
func Mutating(op string) bool {
	switch op {
	case "INSERT", "UPDATE", "DELETE", "UPSERT", "MERGE", "REPLACE":
		return true
	}
	return false
}
