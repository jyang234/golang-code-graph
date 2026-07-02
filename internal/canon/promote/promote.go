// Package promote implements salience filtering as tree contraction (canon spec
// §3.7, plan [C3]). Dropping a sub-threshold internal node is not a delete: the
// node's surviving child-groups are spliced into its parent at the node's own
// position, so order and concurrency membership are preserved. The root (the
// tier-1 entry) is never dropped.
//
// Two shapes matter, both exercised by the fixture:
//   - A sole sequential wrapper (the tier-3 evaluator) is removed and its
//     child-groups take its slot — a concurrent pair under the wrapper surfaces
//     as a concurrent group directly under the root.
//   - A concurrent member that is itself a thin wrapper (the tier-3 scorer around
//     the tier-1 credit-bureau call) is removed and its child promoted into the
//     concurrent group, so the surviving pair stays a race.
package promote

import (
	"fmt"
	"sort"

	"github.com/jyang234/golang-code-graph/ir"
)

// Filter contracts span's subtree in place, dropping every descendant for which
// keep returns false and promoting that node's already-contracted children into
// its parent's slot. span itself (the root) is never dropped. Processing is
// bottom-up: a node's subtree is contracted before the node is considered for
// promotion, so a promoted node always carries only survivors.
func Filter(span *ir.CanonicalSpan, keep func(*ir.CanonicalSpan) bool) {
	if span == nil {
		return
	}
	// Contract every descendant first.
	for gi := range span.Children {
		for _, m := range span.Children[gi].Members {
			Filter(m, keep)
		}
	}

	var out []ir.ChildGroup
	for _, g := range span.Children {
		switch {
		case g.Concurrent:
			out = appendContracted(out, g, keep, groupConcurrent)
		case g.Unordered:
			// Symmetric with Concurrent: post-hoc canonicalization mints
			// multi-member Unordered groups (canon.go), so the old single-member
			// sequential branch below silently processed only Members[0] and dropped
			// the rest — the C-7 false-golden (a tier-1 write erased, or a
			// sub-threshold member leaking in, depending on the op-key sort order).
			out = appendContracted(out, g, keep, groupUnordered)
		default:
			// Sequential group: exactly one member (a happens-before step). Canon
			// never emits a multi-member plain-sequential group; a violation here
			// would mean an unlabeled ordering claim, so fail closed rather than
			// silently process only Members[0] (the shape that hid C-7).
			if len(g.Members) != 1 {
				panic(fmt.Sprintf("promote: sequential group under %q has %d members, want exactly 1", span.Op, len(g.Members)))
			}
			m := g.Members[0]
			if keep(m) {
				out = append(out, g)
				continue
			}
			// Splice the dropped node's child-groups into its slot, preserving order.
			// Carry the dropped group's loop multiplicity onto each spliced child
			// (M-26): the children ran as many times as the contracted wrapper did,
			// so discarding the marker would assert "once" where the truth is "1..*".
			for _, cg := range m.Children {
				out = append(out, withMultiplicity(cg, g.Multiplicity))
			}
		}
	}
	span.Children = out
}

// groupKind selects which unordered-vs-concurrent flag a re-formed multi-member
// group carries after contraction.
type groupKind int

const (
	groupConcurrent groupKind = iota
	groupUnordered
)

// appendContracted applies keep/lossless-promotion to a Concurrent or Unordered
// group and appends the survivors to out. Both kinds share this logic (tenet 5):
// a dropped wrapper's children are promoted into the group only when lossless.
// A concurrent group holds racing branches and an unordered group holds
// order-ambiguous ones; neither is an ordered sequence. So a wrapper is promotable
// only when its surviving subtree is a SINGLE child-group that flattening neither
//   - splices a happens-before sequence into the race (>1 child-group), nor
//   - erases a loop multiplicity ("1..*" → asserted "once" — M-26/R-3), nor
//   - upgrades an ordering claim (an Unordered child flattened into a Concurrent
//     parent would assert a race the child only left order-unknown, and a
//     Concurrent child into an Unordered parent would drop a proven race).
//
// Any wrapper failing that test is RETAINED rather than corrupt the snapshot
// (canon §3.3, plan [C3]). A group that loses all but one member is no longer a
// race/ambiguity and degrades to a plain sequential group.
func appendContracted(out []ir.ChildGroup, g ir.ChildGroup, keep func(*ir.CanonicalSpan) bool, kind groupKind) []ir.ChildGroup {
	members := make([]*ir.CanonicalSpan, 0, len(g.Members))
	for _, m := range g.Members {
		if keep(m) {
			members = append(members, m)
			continue
		}
		switch {
		case len(m.Children) == 0:
			// The wrapper's entire subtree fell below threshold: it disappears.
		case len(m.Children) == 1 && promotableIntoGroup(m.Children[0], kind):
			members = append(members, m.Children[0].Members...)
		default:
			// Promoting would splice a sequence, erase a loop marker, or flip an
			// ordering claim — retain the wrapper so the snapshot keeps asserting
			// only what it can prove (M-26/R-3).
			members = append(members, m)
		}
	}
	switch len(members) {
	case 0:
		// nothing survives
	case 1:
		out = append(out, ir.ChildGroup{Multiplicity: g.Multiplicity, Members: members})
	default:
		sortByOp(members)
		grp := ir.ChildGroup{Multiplicity: g.Multiplicity, Members: members}
		switch kind {
		case groupConcurrent:
			grp.Concurrent = true
		case groupUnordered:
			grp.Unordered = true
		}
		out = append(out, grp)
	}
	return out
}

// promotableIntoGroup reports whether child-group cg can be flattened DIRECTLY
// into a parent group of the given kind without losing or strengthening any
// claim. Safe only when cg carries no loop multiplicity (flattening would erase
// the "1..*" marker — M-26/R-3) AND cg asserts no ordering incompatible with the
// parent: a plain happens-before step joins the race/ambiguity harmlessly, and a
// group of the SAME kind merges associatively; but an Unordered group flattened
// into a Concurrent parent would upgrade "order unknown" to an asserted race, and
// a Concurrent group into an Unordered parent would drop a proven race.
func promotableIntoGroup(cg ir.ChildGroup, kind groupKind) bool {
	if cg.Multiplicity != "" {
		return false
	}
	switch {
	case !cg.Concurrent && !cg.Unordered:
		return true // a plain happens-before step
	case cg.Concurrent && kind == groupConcurrent:
		return true
	case cg.Unordered && kind == groupUnordered:
		return true
	default:
		return false
	}
}

// withMultiplicity returns cg with the outer loop multiplicity applied when cg
// carries none of its own (M-26). The only multiplicity value is "1..*"; a child
// that already carries it keeps it — nested repetition is still "1..*".
func withMultiplicity(cg ir.ChildGroup, outer string) ir.ChildGroup {
	if outer != "" && cg.Multiplicity == "" {
		cg.Multiplicity = outer
	}
	return cg
}

// sortByOp re-sorts contracted concurrent or unordered members by Op after
// promotion. It is a STABLE sort on Op alone, which suffices ONLY because its
// input is already in canon's full canonical order (op + subtree signature):
// canon.group sorts every concurrent/unordered component with bySig before
// promote.Filter runs, and promotion only splices already-canonical subtrees, so
// same-op ties keep that deterministic bySig order here. The stable Op sort
// therefore preserves a run-independent order; it does not by itself impose the
// full canonical key on a same-op tie (bySig owns that upstream). Keep that
// dependency intact: feeding sortByOp a non-bySig-ordered slice would reopen a
// same-op interleaving hole.
func sortByOp(members []*ir.CanonicalSpan) {
	sort.SliceStable(members, func(i, j int) bool { return members[i].Op < members[j].Op })
}
