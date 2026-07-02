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

// TestCorrelationLessDisclosesLostBaggage pins M-18: an in-window span with an
// empty correlation id (a lost baggage context) is excluded from the scoped set,
// and CorrelationLess counts it so the drop is disclosed rather than silent. A span
// belonging to a DIFFERENT run is filtered without being counted (it is not a lost
// context — it is another run's span).
func TestCorrelationLessDisclosesLostBaggage(t *testing.T) {
	spans := []Span{
		{ID: "root", Kind: ir.KindServer, Attrs: map[string]string{CorrelationKey: "mine"}, Start: t0(0), End: t0(10)},
		{ID: "lost", ParentID: "root", Kind: ir.KindClient, Start: t0(1), End: t0(2)},                                    // no correlation id
		{ID: "other", Kind: ir.KindServer, Attrs: map[string]string{CorrelationKey: "theirs"}, Start: t0(0), End: t0(5)}, // another run
	}
	if n := CorrelationLess(spans, "mine"); n != 1 {
		t.Fatalf("CorrelationLess = %d, want 1 (only the empty-correlation span)", n)
	}
	// The scoped set excludes the correlation-less span, so a silent drop is exactly
	// what the count guards against.
	scoped, _ := Scope(spans, "mine")
	if len(scoped) != 1 {
		t.Fatalf("scoped %d spans, want 1 (only the correlated root)", len(scoped))
	}
	// The fast path does no correlation filtering, so nothing is lost.
	if n := CorrelationLess(spans, ""); n != 0 {
		t.Fatalf("CorrelationLess on the runID=\"\" fast path = %d, want 0", n)
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

// TestConcurrentSameWorkerGoroutineSequential is the C-8 regression: two spans on
// the *same* worker goroutine (both != the parent's goroutine, but equal to each
// other) are serialized by construction and must not be classified concurrent —
// even though the old structural branch only compared each to the parent. Their
// disjoint intervals fall through to overlaps(), which is correctly false.
func TestConcurrentSameWorkerGoroutineSequential(t *testing.T) {
	const parent = 1
	a := Span{ID: "a", Goroutine: 7, Start: t0(0), End: t0(1)}
	b := Span{ID: "b", Goroutine: 7, Start: t0(2), End: t0(8)}
	if Concurrent(a, b, parent) {
		t.Error("two sequential spans on one worker goroutine must not be concurrent")
	}
	// Overlapping intervals on the same worker goroutine cannot actually happen
	// (a single goroutine is serialized), but if the clock is coarse enough to
	// report overlap the fallback still reports concurrent — that is the interval
	// signal, not the structural one, and is out of scope for this regression.
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

// TestAgreedStamp pins the one-source code-identity reduction shared by
// ingest.flowStamp and impeach.corpusIdentity: the unique non-empty stamp, with
// ok=false ONLY on a genuine disagreement. ("", true) is "no stamp seen" (skipped
// empties), distinct from ("", false). The two callers layer different
// empty-policies on top of this; the disagreement rule lives only here.
func TestAgreedStamp(t *testing.T) {
	cases := []struct {
		name      string
		stamps    []string
		wantStamp string
		wantOK    bool
	}{
		{"empty-input", nil, "", true},
		{"all-empty", []string{"", ""}, "", true},
		{"single", []string{"c1"}, "c1", true},
		{"agree", []string{"c1", "c1"}, "c1", true},
		{"agree-with-gaps", []string{"", "c1", "", "c1"}, "c1", true},
		{"disagree", []string{"c1", "c2"}, "", false},
		{"disagree-after-gap", []string{"", "c1", "c2"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStamp, gotOK := AgreedStamp(c.stamps)
			if gotStamp != c.wantStamp || gotOK != c.wantOK {
				t.Errorf("AgreedStamp(%v) = (%q,%v), want (%q,%v)", c.stamps, gotStamp, gotOK, c.wantStamp, c.wantOK)
			}
		})
	}
}

// TestAssertableGrade pins the human-assertable capture-fidelity vocabulary: only
// production and integration may be asserted via --capture; synthetic (producer-set,
// never promotes) and the empty string ("not asserted") are not assertions. This is
// the ONE source both the verify CLI and the MCP server validate against, so the
// rejection of a bad grade can never drift between the two boundaries.
func TestAssertableGrade(t *testing.T) {
	cases := []struct {
		grade string
		want  bool
	}{
		{CaptureProduction, true},
		{CaptureIntegration, true},
		{CaptureSynthetic, false},
		{"", false},
		{"staging", false},
		{"Production", false}, // case-sensitive: the vocabulary is exact
	}
	for _, c := range cases {
		if got := AssertableGrade(c.grade); got != c.want {
			t.Errorf("AssertableGrade(%q) = %v, want %v", c.grade, got, c.want)
		}
	}
}
