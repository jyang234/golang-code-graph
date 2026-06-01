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
