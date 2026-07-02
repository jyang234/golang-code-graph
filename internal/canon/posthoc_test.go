package canon

import (
	"testing"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// postHocFlow builds a root server span with three sibling publishes whose
// [start,end] caller-clock intervals (ms) are given by iv. mode selects the
// capture topology. The topics are intentionally not in op-key order.
func postHocFlow(mode capture.CaptureMode, iv [3][2]int) capture.CapturedFlow {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 1000),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x"}},
	}
	for i, topic := range []string{"c.third", "a.first", "b.second"} {
		spans = append(spans, capture.Span{
			ID: topic, ParentID: "root", Kind: ir.KindProducer,
			Start: ms(0, iv[i][0]), End: ms(0, iv[i][1]),
			Attrs: map[string]string{"messaging.destination.name": topic},
		})
	}
	return capture.CapturedFlow{Flow: "x", Service: "s", Mode: mode, Spans: spans, Root: &spans[0], Complete: true}
}

// TestPostHocSequentialRecovery: siblings disjoint by well over the order guard
// (200ms gaps vs the 100ms default) are a reliable happens-before — they must be
// rendered as sequential groups in start order, not concurrent.
func TestPostHocSequentialRecovery(t *testing.T) {
	// c.third@0, a.first@200, b.second@400 — disjoint by ~200ms.
	tr := mustCanon(t, postHocFlow(capture.ModePostHoc, [3][2]int{{0, 1}, {200, 201}, {400, 401}}))
	if len(tr.Root.Children) != 3 {
		t.Fatalf("clearly-disjoint siblings should be 3 sequential groups, got %d:\n%s", len(tr.Root.Children), marshal(t, tr))
	}
	want := []string{"PUBLISH c.third", "PUBLISH a.first", "PUBLISH b.second"} // start order, not op order
	for i, w := range want {
		g := tr.Root.Children[i]
		if g.Concurrent || g.Unordered {
			t.Errorf("group %d should be a plain sequential step, got %+v", i, g)
		}
		if g.Members[0].Op != w {
			t.Errorf("group %d = %q, want %q (start order)", i, g.Members[0].Op, w)
		}
	}
}

// TestPostHocConcurrentOnOverlap: siblings whose intervals overlap are genuine
// parallelism — one concurrent group, members in canonical-key order.
func TestPostHocConcurrentOnOverlap(t *testing.T) {
	tr := mustCanon(t, postHocFlow(capture.ModePostHoc, [3][2]int{{0, 50}, {10, 60}, {20, 70}}))
	if len(tr.Root.Children) != 1 || !tr.Root.Children[0].Concurrent {
		t.Fatalf("overlapping siblings should be one concurrent group, got %+v", tr.Root.Children)
	}
	want := []string{"PUBLISH a.first", "PUBLISH b.second", "PUBLISH c.third"}
	for i, w := range want {
		if tr.Root.Children[0].Members[i].Op != w {
			t.Errorf("member %d = %q, want %q", i, tr.Root.Children[0].Members[i].Op, w)
		}
	}
}

// TestPostHocUnorderedWithinGuard: siblings disjoint by less than the guard
// (9ms gaps) could be coincidentally disjoint — render them unordered, claiming
// neither sequence nor parallelism, but still deterministically (op-key order).
func TestPostHocUnorderedWithinGuard(t *testing.T) {
	tr := mustCanon(t, postHocFlow(capture.ModePostHoc, [3][2]int{{0, 1}, {10, 11}, {20, 21}}))
	if len(tr.Root.Children) != 1 {
		t.Fatalf("within-guard siblings should be one group, got %d:\n%s", len(tr.Root.Children), marshal(t, tr))
	}
	g := tr.Root.Children[0]
	if g.Concurrent || !g.Unordered {
		t.Errorf("group should be unordered (not concurrent), got %+v", g)
	}
	// Determinism is preserved for the ambiguous case: permuting input is identical.
	a := marshal(t, mustCanon(t, postHocFlow(capture.ModePostHoc, [3][2]int{{0, 1}, {10, 11}, {20, 21}})))
	b := marshal(t, mustCanon(t, postHocFlow(capture.ModePostHoc, [3][2]int{{20, 21}, {0, 1}, {10, 11}})))
	if string(a) != string(b) {
		t.Errorf("unordered IR depends on input order:\n a=%s\n b=%s", a, b)
	}
}

// TestPostHocPartialOrdering: a sibling cleanly separated from an ambiguous
// within-guard pair no longer collapses the whole set into one wall-of-par. The
// two near-simultaneous siblings render as one unordered group; the cleanly later
// one renders as its own ordered step after it.
func TestPostHocPartialOrdering(t *testing.T) {
	// c.third@0, a.first@10 (9ms apart — ambiguous), b.second@400 (well separated).
	tr := mustCanon(t, postHocFlow(capture.ModePostHoc, [3][2]int{{0, 1}, {10, 11}, {400, 401}}))
	if len(tr.Root.Children) != 2 {
		t.Fatalf("want 2 groups (one unordered pair, then one sequential step), got %d:\n%s",
			len(tr.Root.Children), marshal(t, tr))
	}
	g0 := tr.Root.Children[0]
	if !g0.Unordered || len(g0.Members) != 2 {
		t.Errorf("group 0 should be the unordered within-guard pair, got %+v", g0)
	}
	g1 := tr.Root.Children[1]
	if g1.Concurrent || g1.Unordered || len(g1.Members) != 1 || g1.Members[0].Op != "PUBLISH b.second" {
		t.Errorf("group 1 should be the cleanly-separated sequential step PUBLISH b.second, got %+v", g1)
	}
}

// TestPostHocUnorderedWhenUntimed: with no timing at all, siblings are unordered
// rather than imposed into an arbitrary deterministic sequence.
func TestPostHocUnorderedWhenUntimed(t *testing.T) {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK,
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x"}},
	}
	for _, topic := range []string{"a", "b"} {
		spans = append(spans, capture.Span{ID: topic, ParentID: "root", Kind: ir.KindProducer,
			Attrs: map[string]string{"messaging.destination.name": topic}})
	}
	tr := mustCanon(t, capture.CapturedFlow{Flow: "x", Service: "s", Mode: capture.ModePostHoc, Spans: spans, Root: &spans[0], Complete: true})
	if len(tr.Root.Children) != 1 || !tr.Root.Children[0].Unordered {
		t.Fatalf("untimed siblings should be one unordered group, got %+v", tr.Root.Children)
	}
}

// TestPostHocSalienceKeepsTier1InUnorderedGroup is the C-7 end-to-end regression:
// a within-guard Unordered group in a post-hoc golden whose FIRST member (by
// canonical key) is a droppable tier-3 compute must not erase the surviving tier-1
// DB/PUBLISH write when salience filtering runs. The old promote.Filter processed
// only Members[0] of a non-concurrent group, silently dropping the rest.
func TestPostHocSalienceKeepsTier1InUnorderedGroup(t *testing.T) {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 1000),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x"}},
		// "aaa-compute" (internal, tier-3, droppable) sorts before "PUBLISH z.topic"
		// by op key; the 4ms gap is within the 100ms guard, so they form one
		// Unordered group with the droppable member first.
		{ID: "c", ParentID: "root", Kind: ir.KindInternal, Name: "aaa-compute", Start: ms(0, 0), End: ms(0, 1)},
		{ID: "p", ParentID: "root", Kind: ir.KindProducer, Start: ms(0, 5), End: ms(0, 6),
			Attrs: map[string]string{"messaging.destination.name": "z.topic"}},
	}
	tr := mustCanon(t, capture.CapturedFlow{Flow: "x", Service: "s", Mode: capture.ModePostHoc, Spans: spans, Root: &spans[0], Complete: true})
	found := false
	for _, g := range tr.Root.Children {
		for _, m := range g.Members {
			switch m.Op {
			case "PUBLISH z.topic":
				found = true
			case "aaa-compute":
				t.Errorf("sub-threshold compute leaked into the golden: %+v", g)
			}
		}
	}
	if !found {
		t.Fatalf("tier-1 PUBLISH erased from post-hoc golden by C-7:\n%s", marshal(t, tr))
	}
}

// TestInProcessStillSequencesSiblings guards that the guard/unordered logic is
// scoped to post-hoc: in-process, disjoint siblings stay sequential in
// happens-before order (the 3-run self-test, not a guard, validates stability).
func TestInProcessStillSequencesSiblings(t *testing.T) {
	tr := mustCanon(t, postHocFlow(capture.ModeInProcess, [3][2]int{{0, 1}, {10, 11}, {20, 21}}))
	if len(tr.Root.Children) != 3 {
		t.Fatalf("in-process disjoint siblings should stay 3 sequential groups, got %d:\n%s",
			len(tr.Root.Children), marshal(t, tr))
	}
	if got := tr.Root.Children[0].Members[0].Op; got != "PUBLISH c.third" {
		t.Errorf("first sequential group = %q, want PUBLISH c.third (earliest start)", got)
	}
}

// TestPostHocTiebreakIsSignatureNotDecodeOrder: two siblings sharing an op but
// with different subtrees order run-independently (by canonical subtree), whether
// they land in a concurrent or unordered group — never by volatile decode order.
func TestPostHocTiebreakIsSignatureNotDecodeOrder(t *testing.T) {
	mk := func(firstChild, secondChild string) capture.CapturedFlow {
		spans := []capture.Span{
			{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 100),
				Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x"}},
			{ID: "c1", ParentID: "root", Kind: ir.KindClient, Start: ms(0, 1), End: ms(0, 5),
				Attrs: map[string]string{"http.request.method": "GET", "peer.service": "api", "http.route": "/v"}},
			{ID: "c1p", ParentID: "c1", Kind: ir.KindProducer, Start: ms(0, 2), End: ms(0, 3),
				Attrs: map[string]string{"messaging.destination.name": firstChild}},
			{ID: "c2", ParentID: "root", Kind: ir.KindClient, Start: ms(0, 10), End: ms(0, 15),
				Attrs: map[string]string{"http.request.method": "GET", "peer.service": "api", "http.route": "/v"}},
			{ID: "c2p", ParentID: "c2", Kind: ir.KindProducer, Start: ms(0, 11), End: ms(0, 12),
				Attrs: map[string]string{"messaging.destination.name": secondChild}},
		}
		return capture.CapturedFlow{Flow: "x", Service: "s", Mode: capture.ModePostHoc, Spans: spans, Root: &spans[0], Complete: true}
	}
	a := marshal(t, mustCanon(t, mk("a.x", "b.y")))
	b := marshal(t, mustCanon(t, mk("b.y", "a.x")))
	if string(a) != string(b) {
		t.Errorf("post-hoc IR depends on decode order for same-op siblings:\n a=%s\n b=%s", a, b)
	}
}
