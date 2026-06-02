package canon

import (
	"testing"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// postHocFlow builds a root server span with three sibling publishes, with the
// siblings' caller-clock intervals supplied by starts (ms). mode selects the
// capture topology so the same spans can be canonicalized in-process vs.
// post-hoc.
func postHocFlow(mode capture.CaptureMode, starts [3]int) capture.CapturedFlow {
	spans := []capture.Span{
		{ID: "root", Kind: ir.KindServer, Status: capture.StatusOK, Start: ms(0, 0), End: ms(0, 100),
			Attrs: map[string]string{"http.request.method": "POST", "http.route": "/x"}},
	}
	for i, topic := range []string{"c.third", "a.first", "b.second"} { // intentionally not op-key order
		spans = append(spans, capture.Span{
			ID: topic, ParentID: "root", Kind: ir.KindProducer,
			Start: ms(0, starts[i]), End: ms(0, starts[i]+1),
			Attrs: map[string]string{"messaging.destination.name": topic},
		})
	}
	return capture.CapturedFlow{Flow: "x", Service: "s", Mode: mode, Spans: spans, Root: &spans[0], Complete: true}
}

// TestPostHocOrdersSiblingsByOpKey: out of process, a parent's children form one
// canonical-key-ordered concurrent group regardless of their caller-clock
// intervals — the in-process path would sequence non-overlapping siblings.
func TestPostHocOrdersSiblingsByOpKey(t *testing.T) {
	// Non-overlapping, strictly sequential starts: in-process this is three
	// sequential groups; post-hoc it must be one concurrent group, op-key ordered.
	tr := mustCanon(t, postHocFlow(capture.ModePostHoc, [3]int{0, 10, 20}))

	if len(tr.Root.Children) != 1 {
		t.Fatalf("post-hoc siblings should be one group, got %d", len(tr.Root.Children))
	}
	g := tr.Root.Children[0]
	if !g.Concurrent {
		t.Errorf("the multi-member sibling group should be flagged concurrent")
	}
	want := []string{"PUBLISH a.first", "PUBLISH b.second", "PUBLISH c.third"}
	if len(g.Members) != len(want) {
		t.Fatalf("got %d members, want %d", len(g.Members), len(want))
	}
	for i, w := range want {
		if g.Members[i].Op != w {
			t.Errorf("member %d = %q, want %q (canonical-key order)", i, g.Members[i].Op, w)
		}
	}
}

// TestPostHocOrderingIsTimingIndependent: permuting the siblings' wall-clock
// intervals (and thus their span order in the flat set) yields byte-identical
// IR — the property a fixed trace file must satisfy regardless of export jitter.
func TestPostHocOrderingIsTimingIndependent(t *testing.T) {
	a := marshal(t, mustCanon(t, postHocFlow(capture.ModePostHoc, [3]int{0, 10, 20})))
	b := marshal(t, mustCanon(t, postHocFlow(capture.ModePostHoc, [3]int{20, 0, 10})))
	c := marshal(t, mustCanon(t, postHocFlow(capture.ModePostHoc, [3]int{5, 5, 5}))) // simultaneous
	if string(a) != string(b) || string(a) != string(c) {
		t.Errorf("post-hoc IR is timing-dependent:\n a=%s\n b=%s\n c=%s", a, b, c)
	}
}

// TestInProcessStillSequencesSiblings guards that the profile is scoped to
// post-hoc: the same strictly-sequential spans captured in-process remain
// distinct sequential groups in happens-before order, unchanged.
func TestInProcessStillSequencesSiblings(t *testing.T) {
	tr := mustCanon(t, postHocFlow(capture.ModeInProcess, [3]int{0, 10, 20}))
	if len(tr.Root.Children) != 3 {
		t.Fatalf("in-process non-overlapping siblings should stay 3 sequential groups, got %d:\n%s",
			len(tr.Root.Children), marshal(t, tr))
	}
	// happens-before order follows the caller clock: c.third started first.
	if got := tr.Root.Children[0].Members[0].Op; got != "PUBLISH c.third" {
		t.Errorf("first sequential group = %q, want PUBLISH c.third (earliest start)", got)
	}
}

// TestPostHocTiebreakIsSignatureNotFileOrder: two siblings sharing an Op but with
// different subtrees must order run-independently (by canonical subtree), not by
// the order they happened to be decoded — otherwise the post-hoc IR/.flow.md
// churns between exports of the same flow.
func TestPostHocTiebreakIsSignatureNotFileOrder(t *testing.T) {
	// Two "HTTP GET api" client siblings with distinct child subtrees (one
	// publishes a.x, the other b.y). Build the flow in both input orders.
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
	// Same flow, the two same-Op clients in swapped decode order.
	a := marshal(t, mustCanon(t, mk("a.x", "b.y")))
	b := marshal(t, mustCanon(t, mk("b.y", "a.x")))
	if string(a) != string(b) {
		t.Errorf("post-hoc IR depends on decode order for same-Op siblings:\n a=%s\n b=%s", a, b)
	}
}
