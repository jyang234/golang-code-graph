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
		if g.Concurrent {
			members := make([]*ir.CanonicalSpan, 0, len(g.Members))
			for _, m := range g.Members {
				if keep(m) {
					members = append(members, m)
					continue
				}
				// Promote the dropped wrapper's children into the race only when it
				// is lossless. A concurrent group holds racing branches, not ordered
				// steps, so a wrapper whose surviving subtree is more than one
				// child-group carries a happens-before sequence that flattening would
				// turn into a race. In that case the node is the minimal structure
				// that preserves the ordering, so we retain it rather than corrupt
				// the snapshot (canon §3.3, plan [C3]). A wrapper with one child-group
				// (its members are a single span or an already-concurrent set)
				// promotes cleanly.
				if len(m.Children) <= 1 {
					for _, cg := range m.Children {
						members = append(members, cg.Members...)
					}
				} else {
					members = append(members, m)
				}
			}
			switch len(members) {
			case 0:
				// nothing survives
			case 1:
				// A race that lost all but one member is no longer a race.
				out = append(out, ir.ChildGroup{Multiplicity: g.Multiplicity, Members: members})
			default:
				sortByOp(members)
				out = append(out, ir.ChildGroup{Concurrent: true, Multiplicity: g.Multiplicity, Members: members})
			}
			continue
		}

		// Sequential group: exactly one member (a happens-before step).
		m := g.Members[0]
		if keep(m) {
			out = append(out, g)
			continue
		}
		// Splice the dropped node's child-groups into its slot, preserving order.
		out = append(out, m.Children...)
	}
	span.Children = out
}

// sortByOp imposes canonical-key order on race-ordered members, so a run's
// interleaving never perturbs the snapshot.
func sortByOp(members []*ir.CanonicalSpan) {
	sort.SliceStable(members, func(i, j int) bool { return members[i].Op < members[j].Op })
}
