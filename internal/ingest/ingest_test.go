package ingest

import (
	"testing"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/otlpjson"
	"github.com/jyang234/golang-code-graph/ir"
)

// span is a terse test-span constructor.
func span(id, parent, slug, svc string, kind ir.Kind, attrs map[string]string) capture.Span {
	a := map[string]string{FlowKey: slug, serviceKey: svc}
	for k, v := range attrs {
		a[k] = v
	}
	return capture.Span{ID: id, ParentID: parent, Kind: kind, Attrs: a}
}

// TestGroupSingleService assembles one trace from one service into one fragment
// rooted at its inbound server span, with no synthesis.
func TestGroupSingleService(t *testing.T) {
	spans := []capture.Span{
		span("1", "", "loan", "loansvc", ir.KindServer, nil),
		span("2", "1", "loan", "loansvc", ir.KindProducer, map[string]string{"messaging.destination.name": "loan.approved"}),
	}
	flows := Group(spans)
	if len(flows) != 1 {
		t.Fatalf("got %d fragments, want 1", len(flows))
	}
	fc := flows[0]
	if fc.Slug != "loan" || fc.Service != "loansvc" {
		t.Errorf("fragment = (%q,%q), want (loan,loansvc)", fc.Slug, fc.Service)
	}
	if fc.Synthesized {
		t.Errorf("inbound server span should be the natural root, not synthesized")
	}
	if fc.Flow.Root == nil || fc.Flow.Root.ID != "1" {
		t.Errorf("root = %+v, want the server span (id 1)", fc.Flow.Root)
	}
	if fc.Flow.Mode != capture.ModePostHoc {
		t.Errorf("mode = %q, want post-hoc", fc.Flow.Mode)
	}
}

// TestGroupPerServiceSplit proves design D-PH4: one slug spanning two services
// yields one fragment per service, each scoped to its own spans.
func TestGroupPerServiceSplit(t *testing.T) {
	spans := []capture.Span{
		span("1", "", "fanout", "publisher", ir.KindServer, nil),
		span("2", "1", "fanout", "publisher", ir.KindProducer, map[string]string{"messaging.destination.name": "loan.approved"}),
		// the subscriber's consume span; its parent (the producer) is in another service.
		span("3", "2", "fanout", "subscriber", ir.KindConsumer, map[string]string{"messaging.destination.name": "loan.approved"}),
	}
	flows := Group(spans)
	if len(flows) != 2 {
		t.Fatalf("got %d fragments, want 2 (one per service)", len(flows))
	}
	// ordered by slug then service: publisher before subscriber.
	if flows[0].Service != "publisher" || flows[1].Service != "subscriber" {
		t.Fatalf("services = (%q,%q), want (publisher,subscriber)", flows[0].Service, flows[1].Service)
	}
	// the subscriber's consume span is parentless within its own fragment (its
	// parent lives in the publisher), so it is the natural consumer root.
	sub := flows[1]
	if sub.Synthesized {
		t.Errorf("a consumer entry span should root the fragment without synthesis")
	}
	if sub.Flow.Trigger != capture.TriggerEvent {
		t.Errorf("subscriber trigger = %q, want event", sub.Flow.Trigger)
	}
}

// TestGroupSynthesizesRootForPublisherOnly: a service that only publishes (no
// inbound entry span in the fragment) gets a synthesized internal root so
// canonicalization sees one tree.
func TestGroupSynthesizesRootForPublisherOnly(t *testing.T) {
	spans := []capture.Span{
		span("1", "remote-parent", "emit", "emitter", ir.KindProducer, map[string]string{"messaging.destination.name": "x.happened"}),
		span("2", "remote-parent", "emit", "emitter", ir.KindProducer, map[string]string{"messaging.destination.name": "y.happened"}),
	}
	flows := Group(spans)
	if len(flows) != 1 {
		t.Fatalf("got %d fragments, want 1", len(flows))
	}
	if !flows[0].Synthesized {
		t.Errorf("two parentless producer spans should force a synthesized root")
	}
	if flows[0].Flow.Root == nil || flows[0].Flow.Root.Kind != ir.KindInternal {
		t.Errorf("synthesized root should be internal, got %+v", flows[0].Flow.Root)
	}
}

func TestGroupIgnoresUntagged(t *testing.T) {
	spans := []capture.Span{
		{ID: "1", Kind: ir.KindServer, Attrs: map[string]string{"service.name": "svc"}}, // no flowmap.flow
	}
	if flows := Group(spans); len(flows) != 0 {
		t.Fatalf("untagged spans should be ignored, got %d fragments", len(flows))
	}
}

// TestIngestPipeline is the end-to-end stage-1 path: decode the committed OTLP
// fixture, group it, canonicalize, and confirm the exercised boundary effects
// are the publish and the outbound dependency — exactly the keys the coverage
// join speaks, derived from a real out-of-process trace shape.
func TestIngestPipeline(t *testing.T) {
	spans, err := otlpjson.DecodeFile("../../testdata/otlp/loan-application.otlp.json")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	flows := Group(spans)
	if len(flows) != 1 {
		t.Fatalf("got %d fragments, want 1", len(flows))
	}
	tr, err := canon.Canonicalize(flows[0].Flow, nil)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	ops := map[string]bool{}
	collect(tr.Root, ops)
	for _, want := range []string{"PUBLISH loan.approved", "HTTP GET credit-bureau /score/{id}"} {
		if !ops[want] {
			t.Errorf("expected exercised boundary op %q; got %v", want, keys(ops))
		}
	}
}

func collect(s *ir.CanonicalSpan, into map[string]bool) {
	if s == nil {
		return
	}
	into[s.Op] = true
	for _, g := range s.Children {
		for _, m := range g.Members {
			collect(m, into)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
