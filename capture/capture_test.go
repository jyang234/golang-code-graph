package capture

import (
	"testing"
	"time"

	"github.com/jyang234/golang-code-graph/ir"
)

func t0(ms int) time.Time { return time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond) }

func TestScopeFiltersByCorrelation(t *testing.T) {
	spans := []Span{
		{ID: "root", Kind: ir.KindServer, Attrs: map[string]string{CorrelationKey: "mine"}, Start: t0(0), End: t0(10)},
		{ID: "child", ParentID: "root", Kind: ir.KindClient, Attrs: map[string]string{CorrelationKey: "mine"}, Start: t0(1), End: t0(2)},
		{ID: "other", Kind: ir.KindServer, Attrs: map[string]string{CorrelationKey: "theirs"}, Start: t0(0), End: t0(5)},
	}
	scoped, root := Scope(spans, "mine")
	if len(scoped) != 2 {
		t.Fatalf("scoped %d spans, want 2 (the foreign run dropped)", len(scoped))
	}
	if root == nil || root.ID != "root" {
		t.Fatalf("root = %+v, want the server span 'root'", root)
	}
}

func TestScopeEmptyRunIDKeepsAll(t *testing.T) {
	spans := []Span{
		{ID: "root", Start: t0(0), End: t0(2)},
		{ID: "child", ParentID: "root", Start: t0(1), End: t0(2)},
	}
	scoped, root := Scope(spans, "")
	if len(scoped) != 2 || root == nil || root.ID != "root" {
		t.Fatalf("in-process fast path: scoped=%d root=%v", len(scoped), root)
	}
}

func TestScopeRejectsMultipleRoots(t *testing.T) {
	spans := []Span{
		{ID: "a", Start: t0(0), End: t0(1)},
		{ID: "b", Start: t0(0), End: t0(1)},
	}
	if _, root := Scope(spans, ""); root != nil {
		t.Fatal("two parentless spans should yield a nil (ambiguous) root")
	}
}

// TestConcurrentOverlapFallback covers the no-goroutine-signal path: ordering
// falls back to caller-clock interval overlap.
func TestConcurrentOverlapFallback(t *testing.T) {
	seqA := Span{ID: "a", Start: t0(0), End: t0(5)}
	seqB := Span{ID: "b", Start: t0(6), End: t0(9)}
	if Concurrent(seqA, seqB, 0) {
		t.Error("disjoint intervals with no goroutine signal should be sequential")
	}
	ovlA := Span{ID: "a", Start: t0(0), End: t0(8)}
	ovlB := Span{ID: "b", Start: t0(2), End: t0(4)}
	if !Concurrent(ovlA, ovlB, 0) {
		t.Error("overlapping intervals should be concurrent")
	}
}

// TestConcurrentStructuralBothAsync is the structural-marker case and the
// regression for the real-DB flake: two siblings dispatched onto worker
// goroutines (each != the parent's goroutine) are concurrent even when their
// intervals are disjoint because one leg finished before the other started.
func TestConcurrentStructuralBothAsync(t *testing.T) {
	const parent = 1
	// a runs fast and finishes before b (on a third goroutine) even starts.
	a := Span{ID: "a", Goroutine: 2, Start: t0(0), End: t0(1)}
	b := Span{ID: "b", Goroutine: 3, Start: t0(2), End: t0(8)}
	if !Concurrent(a, b, parent) {
		t.Error("co-dispatched worker goroutines must be concurrent regardless of interval overlap")
	}
}

// TestConcurrentInlineSiblingSequential covers the fire-and-forget shape: an
// inline call on the parent's goroutine followed by an async span on another
// goroutine is sequential (the async span was spawned after the inline call
// returned), decided by interval since not both are async.
func TestConcurrentInlineSiblingSequential(t *testing.T) {
	const parent = 1
	inline := Span{ID: "ledger", Goroutine: parent, Start: t0(0), End: t0(2)}
	async := Span{ID: "audit", Goroutine: 9, Start: t0(3), End: t0(5)}
	if Concurrent(inline, async, parent) {
		t.Error("an async span spawned after an inline sibling completed must be sequential")
	}
}
