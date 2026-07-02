package promote

import (
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

// keepTier1and2 is the default salience predicate: retain tiers 1–2, drop the
// rest.
func keepTier1and2(s *ir.CanonicalSpan) bool { return s.Tier <= 2 }

func seq(members ...*ir.CanonicalSpan) ir.ChildGroup {
	return ir.ChildGroup{Members: members}
}
func conc(members ...*ir.CanonicalSpan) ir.ChildGroup {
	return ir.ChildGroup{Concurrent: true, Members: members}
}
func unord(members ...*ir.CanonicalSpan) ir.ChildGroup {
	return ir.ChildGroup{Unordered: len(members) > 1, Members: members}
}
func seqM(mult string, members ...*ir.CanonicalSpan) ir.ChildGroup {
	return ir.ChildGroup{Multiplicity: mult, Members: members}
}
func span(op string, tier int, kids ...ir.ChildGroup) *ir.CanonicalSpan {
	return &ir.CanonicalSpan{Op: op, Tier: tier, Children: kids}
}

// TestSoleWrapperPromotesConcurrentPair is the evaluator case: a tier-3 internal
// node whose only content is a concurrent pair is dropped, and the pair appears
// directly under the root as a concurrent group in the wrapper's slot.
func TestSoleWrapperPromotesConcurrentPair(t *testing.T) {
	root := span("HTTP POST /loan-application", 1,
		seq(span("evaluateApplication", 3,
			conc(
				span("DB postgresql SELECT applicants", 2),
				span("HTTP GET credit-bureau /score/{id}", 1),
			),
		)),
		seq(span("HTTP POST payment-gw /charge/{id}", 1)),
	)
	Filter(root, keepTier1and2)

	if len(root.Children) != 2 {
		t.Fatalf("root should have 2 groups (promoted pair + charge), got %d", len(root.Children))
	}
	g0 := root.Children[0]
	if !g0.Concurrent || len(g0.Members) != 2 {
		t.Fatalf("first group should be the promoted concurrent pair, got %+v", g0)
	}
	// Canonical-key order: DB sorts before HTTP.
	if g0.Members[0].Op != "DB postgresql SELECT applicants" || g0.Members[1].Op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("concurrent members not in canonical-key order: %q, %q", g0.Members[0].Op, g0.Members[1].Op)
	}
	if root.Children[1].Members[0].Op != "HTTP POST payment-gw /charge/{id}" {
		t.Errorf("second group lost: %+v", root.Children[1])
	}
}

// TestConcurrentMemberWrapperPromoted is the scorer case: a thin tier-3 wrapper
// that is itself one leg of a concurrent pair is dropped, and its single child
// stays in the race alongside the sibling.
func TestConcurrentMemberWrapperPromoted(t *testing.T) {
	root := span("root", 1,
		conc(
			span("DB postgresql SELECT applicants", 2),
			span("scorer.Score", 3,
				seq(span("HTTP GET credit-bureau /score/{id}", 1)),
			),
		),
	)
	Filter(root, keepTier1and2)

	if len(root.Children) != 1 {
		t.Fatalf("expected one concurrent group, got %d", len(root.Children))
	}
	g := root.Children[0]
	if !g.Concurrent || len(g.Members) != 2 {
		t.Fatalf("expected the surviving pair to stay concurrent, got %+v", g)
	}
	if g.Members[0].Op != "DB postgresql SELECT applicants" || g.Members[1].Op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("promoted members = %q, %q", g.Members[0].Op, g.Members[1].Op)
	}
}

// TestConcurrentMemberWrapperKeepsSequentialChildren is the regression for the
// flatten bug: a dropped concurrent member whose subtree is an ordered sequence
// (ledger then audit) must NOT have its children spliced into the race, which
// would assert a happens-before pair as concurrent. The wrapper is retained as
// the minimal structure that preserves the ordering.
func TestConcurrentMemberWrapperKeepsSequentialChildren(t *testing.T) {
	root := span("root", 1,
		conc(
			span("DB postgresql SELECT applicants", 2),
			span("disburse", 3, // tier-3 wrapper with an ORDERED sub-sequence
				seq(span("DB postgres INSERT ledger", 1)),
				seq(span("DB postgres INSERT audit", 1)),
			),
		),
	)
	Filter(root, keepTier1and2)

	if len(root.Children) != 1 || !root.Children[0].Concurrent {
		t.Fatalf("expected one concurrent group, got %+v", root.Children)
	}
	members := root.Children[0].Members
	if len(members) != 2 {
		t.Fatalf("expected the SELECT and the retained wrapper, got %d members", len(members))
	}
	// The wrapper is retained (sorted by op: "DB ..." < "disburse").
	wrapper := members[1]
	if wrapper.Op != "disburse" {
		t.Fatalf("expected the wrapper retained to preserve order, got %q", wrapper.Op)
	}
	if len(wrapper.Children) != 2 ||
		wrapper.Children[0].Members[0].Op != "DB postgres INSERT ledger" ||
		wrapper.Children[1].Members[0].Op != "DB postgres INSERT audit" {
		t.Errorf("ledger-before-audit ordering lost: %+v", wrapper.Children)
	}
	if wrapper.Children[0].Concurrent || wrapper.Children[1].Concurrent {
		t.Error("sequential children must not have become a race")
	}
}

// TestSequentialChainContraction drops a tier-3 node in a sequential chain and
// splices its sequential children into the parent in order.
func TestSequentialChainContraction(t *testing.T) {
	root := span("root", 1,
		seq(span("disburse", 3,
			seq(span("DB postgres INSERT ledger", 1)),
			seq(span("DB postgres INSERT audit", 1)),
		)),
	)
	Filter(root, keepTier1and2)
	if len(root.Children) != 2 {
		t.Fatalf("expected ledger then audit promoted as two sequential groups, got %d", len(root.Children))
	}
	if root.Children[0].Members[0].Op != "DB postgres INSERT ledger" ||
		root.Children[1].Members[0].Op != "DB postgres INSERT audit" {
		t.Errorf("sequential order not preserved: %q then %q",
			root.Children[0].Members[0].Op, root.Children[1].Members[0].Op)
	}
}

// TestUnorderedDropDoesNotEraseSurvivor is the C-7 regression: an Unordered group
// whose FIRST member is a droppable tier-3 node must not erase the surviving
// tier-1 member. The old code processed only Members[0] under the sequential
// branch, splicing the tier-3's (empty) children and silently dropping the tier-1
// DB write from a freshly-minted golden.
func TestUnorderedDropDoesNotEraseSurvivor(t *testing.T) {
	root := span("root", 1,
		unord(
			span("Auth.check", 3),                // tier-3, drops, sorts first
			span("DB postgres INSERT ledger", 1), // tier-1, must survive
		),
	)
	Filter(root, keepTier1and2)
	if len(root.Children) != 1 {
		t.Fatalf("tier-1 write erased from unordered group: got %d groups %+v", len(root.Children), root.Children)
	}
	if got := root.Children[0].Members[0].Op; got != "DB postgres INSERT ledger" {
		t.Errorf("surviving member = %q, want the tier-1 DB write", got)
	}
	// One survivor is no longer an ambiguity: it degrades to a plain sequential group.
	if root.Children[0].Unordered {
		t.Error("single-survivor group should no longer be marked Unordered")
	}
}

// TestUnorderedKeepFirstDropsRest is the mirror C-7 case: an Unordered group whose
// FIRST member is kept must still DROP the sub-threshold rest, not leak the whole
// group. The old keep-first branch appended the entire group unchanged.
func TestUnorderedKeepFirstDropsRest(t *testing.T) {
	root := span("root", 1,
		unord(
			span("DB postgres INSERT ledger", 1), // tier-1, kept, sorts first
			span("internalCompute", 3),           // tier-3, must be dropped
		),
	)
	Filter(root, keepTier1and2)
	if len(root.Children) != 1 || len(root.Children[0].Members) != 1 {
		t.Fatalf("sub-threshold member leaked into golden: %+v", root.Children)
	}
	if got := root.Children[0].Members[0].Op; got != "DB postgres INSERT ledger" {
		t.Errorf("kept member = %q, want the tier-1 DB write", got)
	}
}

// TestUnorderedWrapperPromotedLossless drops a thin tier-3 wrapper inside an
// unordered group and promotes its single child-group's members into the group,
// keeping the group unordered and re-sorted.
func TestUnorderedWrapperPromotedLossless(t *testing.T) {
	root := span("root", 1,
		unord(
			span("HTTP GET /a", 1),
			span("wrapper", 3, seq(span("DB postgres INSERT b", 1))),
		),
	)
	Filter(root, keepTier1and2)
	if len(root.Children) != 1 {
		t.Fatalf("expected one unordered group, got %+v", root.Children)
	}
	g := root.Children[0]
	if !g.Unordered || len(g.Members) != 2 {
		t.Fatalf("expected unordered pair after lossless promotion, got %+v", g)
	}
	if g.Members[0].Op >= g.Members[1].Op {
		t.Errorf("promoted members not re-sorted by op: %q, %q", g.Members[0].Op, g.Members[1].Op)
	}
}

// TestSplicedChildInheritsMultiplicity is the M-26 regression: dropping a wrapper
// whose sequential group carried a "1..*" loop marker must propagate that marker
// onto the spliced children, not silently assert "once".
func TestSplicedChildInheritsMultiplicity(t *testing.T) {
	root := span("root", 1,
		seqM("1..*", span("perItem", 3,
			seq(span("DB postgres INSERT ledger", 1)),
		)),
	)
	Filter(root, keepTier1and2)
	if len(root.Children) != 1 {
		t.Fatalf("expected the spliced child, got %+v", root.Children)
	}
	if root.Children[0].Multiplicity != "1..*" {
		t.Errorf("loop multiplicity discarded on splice: %+v", root.Children[0])
	}
}

// hasMultiplicity reports whether any group in span's subtree carries mult.
func hasMultiplicity(span *ir.CanonicalSpan, mult string) bool {
	for _, g := range span.Children {
		if g.Multiplicity == mult {
			return true
		}
		for _, m := range g.Members {
			if hasMultiplicity(m, mult) {
				return true
			}
		}
	}
	return false
}

// TestConcurrentWrapperKeepsLoopMarker is the R-3 regression: dropping a wrapper
// whose sole child-group carries a "1..*" loop marker into a CONCURRENT group must
// NOT flatten the child and discard the marker — that would assert the INSERT ran
// exactly once where the truth is 1..*, M-26's defect on the C-7 promotion path.
// The wrapper is retained so the marker survives.
func TestConcurrentWrapperKeepsLoopMarker(t *testing.T) {
	root := span("root", 1,
		conc(
			span("HTTP GET /a", 1),
			span("wrapper", 3, seqM("1..*", span("DB postgres INSERT ledger", 1))),
		),
	)
	Filter(root, keepTier1and2)

	if !hasMultiplicity(root, "1..*") {
		t.Fatalf("loop multiplicity discarded on concurrent promotion: %+v", root.Children)
	}
	// The INSERT must not appear as a bare, marker-less DIRECT member of the
	// concurrent group (which would assert "runs once").
	for _, g := range root.Children {
		if !g.Concurrent {
			continue
		}
		for _, m := range g.Members {
			if m.Op == "DB postgres INSERT ledger" {
				t.Errorf("INSERT promoted into the concurrent group bare, erasing its 1..* marker: %+v", g)
			}
		}
	}
}

// TestUnorderedWrapperKeepsLoopMarker is the R-3 regression on the UNORDERED branch
// (appendContracted is invoked separately per group kind): dropping a wrapper whose
// sole child-group carries a "1..*" marker into an UNORDERED group must likewise not
// flatten-and-discard the marker.
func TestUnorderedWrapperKeepsLoopMarker(t *testing.T) {
	root := span("root", 1,
		unord(
			span("HTTP GET /a", 1),
			span("wrapper", 3, seqM("1..*", span("DB postgres INSERT ledger", 1))),
		),
	)
	Filter(root, keepTier1and2)

	if !hasMultiplicity(root, "1..*") {
		t.Fatalf("loop multiplicity discarded on unordered promotion: %+v", root.Children)
	}
	for _, g := range root.Children {
		if !g.Unordered {
			continue
		}
		for _, m := range g.Members {
			if m.Op == "DB postgres INSERT ledger" {
				t.Errorf("INSERT promoted into the unordered group bare, erasing its 1..* marker: %+v", g)
			}
		}
	}
}

// TestConcurrentWrapperKeepsUnorderedChild is the second R-3 sub-case: an Unordered
// child-group flattened into a Concurrent parent would UPGRADE "order unknown" to
// an asserted race. The wrapper must be retained so the unordered relationship is
// preserved, not strengthened.
func TestConcurrentWrapperKeepsUnorderedChild(t *testing.T) {
	root := span("root", 1,
		conc(
			span("HTTP GET /a", 1),
			span("wrapper", 3, unord(
				span("DB postgres INSERT b", 1),
				span("DB postgres INSERT c", 1),
			)),
		),
	)
	Filter(root, keepTier1and2)

	// The two INSERTs must still be under an Unordered group somewhere, not hoisted
	// as direct racing members of the top concurrent group.
	var foundUnordered bool
	var walk func(*ir.CanonicalSpan)
	walk = func(s *ir.CanonicalSpan) {
		for _, g := range s.Children {
			if g.Unordered && len(g.Members) == 2 {
				foundUnordered = true
			}
			for _, m := range g.Members {
				walk(m)
			}
		}
	}
	walk(root)
	if !foundUnordered {
		t.Errorf("unordered child upgraded to a concurrent race on promotion: %+v", root.Children)
	}
}

// TestRootNeverDropped guards the invariant that the entry stays even if its tier
// somehow exceeds the threshold.
func TestRootNeverDropped(t *testing.T) {
	root := span("root", 4, seq(span("child", 1)))
	Filter(root, keepTier1and2)
	if root.Op != "root" {
		t.Fatal("root must never be dropped")
	}
	if len(root.Children) != 1 || root.Children[0].Members[0].Op != "child" {
		t.Errorf("kept child lost: %+v", root.Children)
	}
}

// TestLeafDropLeavesEmpty drops a tier-4 leaf with no children, leaving no group.
func TestLeafDropLeavesEmpty(t *testing.T) {
	root := span("root", 1, seq(span("log.debug", 4)))
	Filter(root, keepTier1and2)
	if len(root.Children) != 0 {
		t.Errorf("dropped leaf should leave no group, got %+v", root.Children)
	}
}
