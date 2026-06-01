package diff

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

func sp(op string, kind ir.Kind, peer string, kids ...ir.ChildGroup) *ir.CanonicalSpan {
	return &ir.CanonicalSpan{Op: op, Kind: kind, Peer: peer, Tier: 1, Children: kids}
}
func seq(m ...*ir.CanonicalSpan) ir.ChildGroup  { return ir.ChildGroup{Members: m} }
func conc(m ...*ir.CanonicalSpan) ir.ChildGroup { return ir.ChildGroup{Concurrent: true, Members: m} }
func tr(root *ir.CanonicalSpan) *ir.CanonicalTrace {
	return &ir.CanonicalTrace{Flow: "f", Service: "loansvc", Root: root}
}

func root(kids ...ir.ChildGroup) *ir.CanonicalSpan {
	return sp("HTTP POST /loan-application", ir.KindServer, "", kids...)
}

func lines(cs []Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.String()
	}
	return out
}

func TestNoChange(t *testing.T) {
	a := tr(root(seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus"))))
	b := tr(root(seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus"))))
	if got := Diff(a, b); len(got) != 0 {
		t.Fatalf("identical traces should diff to empty, got %v", lines(got))
	}
}

func TestAddedContractPublish(t *testing.T) {
	a := tr(root(seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus"))))
	b := tr(root(
		seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus")),
		seq(sp("PUBLISH disbursement.initiated", ir.KindProducer, "Bus")),
	))
	got := Diff(a, b)
	if len(got) != 1 || got[0].Type != Added || got[0].Priority != PriorityContract {
		t.Fatalf("want one contract Added, got %v", lines(got))
	}
	if !strings.Contains(got[0].String(), "[CONTRACT] ADDED PUBLISH disbursement.initiated") {
		t.Errorf("line = %q", got[0].String())
	}
}

func TestRemovedContractDependency(t *testing.T) {
	a := tr(root(seq(sp("HTTP GET credit-bureau /score/{id}", ir.KindClient, "credit-bureau"))))
	b := tr(root())
	got := Diff(a, b)
	if len(got) != 1 || got[0].Type != Removed || got[0].Priority != PriorityContract {
		t.Fatalf("want one contract Removed, got %v", lines(got))
	}
	if !strings.Contains(got[0].String(), "[CONTRACT] REMOVED GET credit-bureau /score/{id}") {
		t.Errorf("line = %q", got[0].String())
	}
}

func TestStatusChangeIsTier1(t *testing.T) {
	old := sp("HTTP POST payment-gw /charge/{id}", ir.KindClient, "payment-gw")
	old.Status = "ok"
	neu := sp("HTTP POST payment-gw /charge/{id}", ir.KindClient, "payment-gw")
	neu.Status, neu.ErrorType = "error", "timeout"

	got := Diff(tr(root(seq(old))), tr(root(seq(neu))))
	if len(got) == 0 {
		t.Fatal("expected status/error changes")
	}
	joined := strings.Join(lines(got), "\n")
	if !strings.Contains(joined, "[T1]") || !strings.Contains(joined, "status ok→error") {
		t.Errorf("want a tier-1 status change, got:\n%s", joined)
	}
}

func TestConcurrencyChanged(t *testing.T) {
	// golden: SELECT and credit-bureau sequential; new: concurrent.
	a := tr(root(
		seq(sp("DB postgres SELECT applicants", ir.KindClient, "postgres")),
		seq(sp("HTTP GET credit-bureau /score/{id}", ir.KindClient, "credit-bureau")),
	))
	b := tr(root(
		conc(
			sp("DB postgres SELECT applicants", ir.KindClient, "postgres"),
			sp("HTTP GET credit-bureau /score/{id}", ir.KindClient, "credit-bureau"),
		),
	))
	got := Diff(a, b)
	if !anyType(got, ConcurrencyChanged) {
		t.Fatalf("expected ConcurrencyChanged, got %v", lines(got))
	}
	for _, c := range got {
		if c.Type == ConcurrencyChanged && !strings.HasPrefix(c.String(), "[CONCURRENCY]") {
			t.Errorf("bad prefix: %q", c.String())
		}
	}
}

func TestCardinalityChanged(t *testing.T) {
	a := tr(root(ir.ChildGroup{Members: []*ir.CanonicalSpan{sp("DB postgres INSERT items", ir.KindClient, "postgres")}}))
	b := tr(root(ir.ChildGroup{Multiplicity: "1..*", Members: []*ir.CanonicalSpan{sp("DB postgres INSERT items", ir.KindClient, "postgres")}}))
	got := Diff(a, b)
	if !anyType(got, CardinalityChanged) {
		t.Fatalf("expected CardinalityChanged, got %v", lines(got))
	}
	if !strings.Contains(strings.Join(lines(got), "\n"), "multiplicity 1→1..*") {
		t.Errorf("want multiplicity detail, got %v", lines(got))
	}
}

func TestAttrChangeRanksLow(t *testing.T) {
	old := sp("DB postgres SELECT applicants", ir.KindClient, "postgres")
	old.Attrs = map[string]string{"db.statement": "SELECT a FROM applicants WHERE id = ?"}
	neu := sp("DB postgres SELECT applicants", ir.KindClient, "postgres")
	neu.Attrs = map[string]string{"db.statement": "SELECT a , b FROM applicants WHERE id = ?"}
	got := Diff(tr(root(seq(old))), tr(root(seq(neu))))
	if len(got) != 1 || got[0].Priority != PriorityLower {
		t.Fatalf("want one low-priority attr change, got %v", lines(got))
	}
	if !strings.HasPrefix(got[0].String(), "[MINOR]") {
		t.Errorf("line = %q", got[0].String())
	}
}

// TestFraudScreeningPrioritization reproduces the artifacts §6 PR: a new
// fraud-svc dependency is added and a sibling is reordered. The contract change
// must rank before the reorder.
func TestFraudScreeningPrioritization(t *testing.T) {
	a := tr(root(
		seq(sp("DB postgres SELECT applicants", ir.KindClient, "postgres")),
		seq(sp("HTTP POST payment-gw /charge/{id}", ir.KindClient, "payment-gw")),
		seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus")),
	))
	b := tr(root(
		seq(sp("DB postgres SELECT applicants", ir.KindClient, "postgres")),
		seq(sp("HTTP GET fraud-svc /check/{id}", ir.KindClient, "fraud-svc")), // ADDED contract
		seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus")),              // reordered before charge
		seq(sp("HTTP POST payment-gw /charge/{id}", ir.KindClient, "payment-gw")),
	))
	got := Diff(a, b)

	contractIdx, reorderIdx := -1, -1
	for i, c := range got {
		if c.Type == Added && strings.Contains(c.String(), "fraud-svc") {
			contractIdx = i
		}
		if c.Type == Reordered && reorderIdx == -1 {
			reorderIdx = i
		}
	}
	if contractIdx == -1 {
		t.Fatalf("missing fraud-svc contract add in %v", lines(got))
	}
	if reorderIdx == -1 {
		t.Fatalf("missing reorder in %v", lines(got))
	}
	if contractIdx > reorderIdx {
		t.Errorf("contract change must precede reorder:\n%v", lines(got))
	}
	if !strings.Contains(got[contractIdx].String(), "[CONTRACT] ADDED GET fraud-svc /check/{id}") {
		t.Errorf("contract line = %q", got[contractIdx].String())
	}
}

// TestLISMinimalReorder moves one sibling among five; only that one is reported
// reordered (LIS), not a delete/add cascade of the rest.
func TestLISMinimalReorder(t *testing.T) {
	mk := func(order ...string) *ir.CanonicalTrace {
		groups := make([]ir.ChildGroup, len(order))
		for i, name := range order {
			groups[i] = seq(sp(name, ir.KindInternal, ""))
		}
		return tr(root(groups...))
	}
	a := mk("a", "b", "c", "d", "e")
	b := mk("a", "c", "d", "e", "b") // b moved to the end
	got := Diff(a, b)

	var reordered []string
	for _, c := range got {
		if c.Type == Reordered {
			reordered = append(reordered, c.Op)
		}
	}
	if len(reordered) != 1 || reordered[0] != "b" {
		t.Fatalf("LIS should report only 'b' moved, got %v (all: %v)", reordered, lines(got))
	}
}

func TestAddedNonContractIsLowOrTier1(t *testing.T) {
	// An internal compute node added: not a contract; tier here is 1 by builder,
	// so it is tier-1, but it must not be classified as a contract change.
	a := tr(root())
	b := tr(root(seq(sp("auditLog", ir.KindInternal, ""))))
	got := Diff(a, b)
	if len(got) != 1 || got[0].Priority == PriorityContract {
		t.Fatalf("internal add must not be a contract change, got %v", lines(got))
	}
}

func anyType(cs []Change, t Type) bool {
	for _, c := range cs {
		if c.Type == t {
			return true
		}
	}
	return false
}
