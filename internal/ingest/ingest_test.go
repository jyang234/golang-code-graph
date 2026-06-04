package ingest

import (
	"encoding/json"
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

// TestRealCollectorSampleEffects ties the authoritative collector-format sample
// to the gate path: decoding real ptrace.JSONMarshaler output, grouping, and
// canonicalizing recovers the same boundary-effect set the in-process golden
// asserts — so the decoder is pinned to collector output end to end.
func TestRealCollectorSampleEffects(t *testing.T) {
	spans, err := otlpjson.DecodeFile("../../testdata/otlp/loansvc.collector.otlp.json")
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
	got := BoundaryEffects(tr.Root)
	want := []string{
		"HTTP GET credit-bureau /score/{id}",
		"HTTP POST /loan-application",
		"HTTP POST payment-gw /charge/{id}",
		"PUBLISH loan.approved",
	}
	if !equalStrs(got, want) {
		t.Errorf("boundary effects from the real sample = %v, want %v", got, want)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestWholeFlowsKeepsCrossService: WholeFlows (the render unit) keeps one
// cross-service tree where Group (the gate unit) splits per service.
func TestWholeFlowsKeepsCrossService(t *testing.T) {
	spans := []capture.Span{
		span("1", "", "loan", "loansvc", ir.KindServer, nil),
		span("2", "1", "loan", "loansvc", ir.KindClient, map[string]string{"peer.service": "credit-bureau", "http.request.method": "GET", "http.route": "/score/{id}"}),
		span("3", "2", "loan", "credit-bureau", ir.KindServer, map[string]string{"http.request.method": "GET", "http.route": "/score/{id}"}),
		span("4", "1", "loan", "loansvc", ir.KindProducer, map[string]string{"messaging.destination.name": "loan.approved"}),
		span("5", "4", "loan", "notifier", ir.KindConsumer, map[string]string{"messaging.destination.name": "loan.approved"}),
	}
	if g := Group(spans); len(g) != 3 {
		t.Fatalf("Group should split per service: got %d, want 3", len(g))
	}
	wf := WholeFlows(spans)
	if len(wf) != 1 {
		t.Fatalf("WholeFlows: got %d, want 1 cross-service tree", len(wf))
	}
	f := wf[0]
	if f.Service != "loansvc" {
		t.Errorf("entry service = %q, want loansvc", f.Service)
	}
	if f.Synthesized {
		t.Errorf("single server entry should root without synthesis")
	}
	if f.Flow.Root == nil || f.Flow.Root.ID != "1" {
		t.Errorf("root should be the loansvc entry (id 1), got %+v", f.Flow.Root)
	}
	if len(f.Flow.Spans) != 5 {
		t.Errorf("whole-flow should keep all 5 spans across services, got %d", len(f.Flow.Spans))
	}
}

// brokerFlow builds a two-trace async flow: trace A is the producer side and
// carries the flow baggage; trace B is the consumer side — a fresh trace with no
// flow baggage and no parent reaching back to A, joined only by a FOLLOWS_FROM
// span link from the consumer entry to the producer span it processed.
func brokerFlow() []capture.Span {
	return []capture.Span{
		{TraceID: "A", ID: "s1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "loan", serviceKey: "loansvc", "http.request.method": "POST", "http.route": "/loan"}},
		{TraceID: "A", ID: "p1", ParentID: "s1", Kind: ir.KindProducer,
			Attrs: map[string]string{FlowKey: "loan", serviceKey: "loansvc", "messaging.destination.name": "loan.approved"}},
		{TraceID: "B", ID: "c1", Kind: ir.KindConsumer,
			Attrs: map[string]string{serviceKey: "notifier", "messaging.destination.name": "loan.approved"},
			Links: []capture.SpanLink{{TraceID: "A", SpanID: "p1"}}},
		{TraceID: "B", ID: "n1", ParentID: "c1", Kind: ir.KindClient,
			Attrs: map[string]string{serviceKey: "notifier", "http.request.method": "POST", "peer.service": "email", "http.route": "/send"}},
	}
}

// TestStitchJoinsConsumerByLink: a consumer trace that never carried the flow
// baggage (it correctly did not cross the broker) still joins the flow via its
// span link, and Group gates it as its own per-service fragment.
func TestStitchJoinsConsumerByLink(t *testing.T) {
	g := Group(brokerFlow())
	if len(g) != 2 {
		t.Fatalf("Group: got %d fragments, want 2 (loansvc, notifier); services=%v", len(g), services(g))
	}
	var notifier *FlowCapture
	for i := range g {
		if g[i].Service == "notifier" {
			notifier = &g[i]
		}
	}
	if notifier == nil {
		t.Fatalf("consumer did not join the flow via link; services=%v", services(g))
	}
	if notifier.Slug != "loan" {
		t.Errorf("recovered fragment slug = %q, want loan", notifier.Slug)
	}
	if notifier.Synthesized {
		t.Errorf("consumer entry span should root the fragment without synthesis")
	}
	if notifier.Flow.Trigger != capture.TriggerEvent {
		t.Errorf("consumer fragment trigger = %q, want event", notifier.Flow.Trigger)
	}
}

// TestStitchWholeFlowReparentsAcrossBroker: WholeFlows reparents the link-joined
// consumer onto the producer, yielding one connected cross-trace tree rather
// than two roots that would force synthesis.
func TestStitchWholeFlowReparentsAcrossBroker(t *testing.T) {
	wf := WholeFlows(brokerFlow())
	if len(wf) != 1 {
		t.Fatalf("WholeFlows: got %d, want 1 stitched cross-trace tree", len(wf))
	}
	f := wf[0]
	if f.Service != "loansvc" {
		t.Errorf("entry service = %q, want loansvc", f.Service)
	}
	if f.Synthesized {
		t.Errorf("link stitch should connect consumer to producer; root must not be synthesized")
	}
	if len(f.Flow.Spans) != 4 {
		t.Errorf("stitched whole flow should keep all 4 spans, got %d", len(f.Flow.Spans))
	}
	var consumer *capture.Span
	for i := range f.Flow.Spans {
		if f.Flow.Spans[i].Kind == ir.KindConsumer {
			consumer = &f.Flow.Spans[i]
		}
	}
	if consumer == nil {
		t.Fatal("consumer span missing from stitched flow")
	}
	if consumer.ParentID != skey("A", "p1") {
		t.Errorf("consumer parent = %q, want producer global id %q", consumer.ParentID, skey("A", "p1"))
	}
	// The link-reparented consumer is marked async so the renderer can draw it as a
	// distinct asynchronous hop rather than a synchronous nested call.
	if !consumer.AsyncLink {
		t.Errorf("link-stitched consumer should be marked AsyncLink")
	}
}

// TestStitchNewRootGateIgnoresMidTraceLink: a span that already has an in-trace
// parent but also carries a link (a causal reference that is not its parent)
// keeps its real parent — only genuine new roots follow links.
func TestStitchNewRootGateIgnoresMidTraceLink(t *testing.T) {
	spans := []capture.Span{
		{TraceID: "A", ID: "s1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "loan", serviceKey: "svc", "http.request.method": "POST", "http.route": "/x"}},
		{TraceID: "A", ID: "m1", ParentID: "s1", Kind: ir.KindInternal,
			Attrs: map[string]string{FlowKey: "loan", serviceKey: "svc"},
			Links: []capture.SpanLink{{TraceID: "A", SpanID: "s1"}}},
	}
	for _, s := range stitch(spans) {
		if s.ID == skey("A", "m1") && s.ParentID != skey("A", "s1") {
			t.Errorf("mid-trace span reparented by link: parent = %q, want %q", s.ParentID, skey("A", "s1"))
		}
	}
}

// TestStitchPropagatesMembershipMultiHop: slug membership flows across a
// produce→consume→produce→consume chain of three traces to a fixpoint, so a
// downstream consumer two hops from the tagged entry still joins the flow.
func TestStitchPropagatesMembershipMultiHop(t *testing.T) {
	spans := []capture.Span{
		// Trace A: tagged entry publishes.
		{TraceID: "A", ID: "s1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "loan", serviceKey: "svc-a", "http.request.method": "POST", "http.route": "/x"}},
		{TraceID: "A", ID: "p1", ParentID: "s1", Kind: ir.KindProducer,
			Attrs: map[string]string{FlowKey: "loan", serviceKey: "svc-a", "messaging.destination.name": "t1"}},
		// Trace B: consumes t1 (link to p1), republishes to t2.
		{TraceID: "B", ID: "c1", Kind: ir.KindConsumer,
			Attrs: map[string]string{serviceKey: "svc-b", "messaging.destination.name": "t1"},
			Links: []capture.SpanLink{{TraceID: "A", SpanID: "p1"}}},
		{TraceID: "B", ID: "p2", ParentID: "c1", Kind: ir.KindProducer,
			Attrs: map[string]string{serviceKey: "svc-b", "messaging.destination.name": "t2"}},
		// Trace C: consumes t2 (link to p2) — two hops from the tagged entry.
		{TraceID: "C", ID: "c2", Kind: ir.KindConsumer,
			Attrs: map[string]string{serviceKey: "svc-c", "messaging.destination.name": "t2"},
			Links: []capture.SpanLink{{TraceID: "B", SpanID: "p2"}}},
	}
	g := Group(spans)
	if got := services(g); len(got) != 3 {
		t.Fatalf("multi-hop chain should yield 3 fragments, got %v", got)
	}
	for _, fc := range g {
		if fc.Slug != "loan" {
			t.Errorf("fragment %q slug = %q, want loan (membership did not propagate)", fc.Service, fc.Slug)
		}
	}
}

// TestIngestAsyncBrokerFixture is the end-to-end async path over a committed
// two-trace OTLP export: the notifier service consumes loan.approved on its own
// trace, carrying no flow baggage and only a span link back to the producer.
// Decoding + grouping must recover its membership from the link and gate it as
// its own fragment, whose exercised boundary effect is its outbound HTTP call.
func TestIngestAsyncBrokerFixture(t *testing.T) {
	spans, err := otlpjson.DecodeFile("../../testdata/otlp/async-broker.otlp.json")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	g := Group(spans)
	if len(g) != 2 {
		t.Fatalf("got %d fragments, want 2 (loansvc, notifier); services=%v", len(g), services(g))
	}
	var notifier *FlowCapture
	for i := range g {
		if g[i].Service == "notifier" {
			notifier = &g[i]
		}
	}
	if notifier == nil {
		t.Fatalf("notifier did not join the flow via span link; services=%v", services(g))
	}
	if notifier.Slug != "loan-application" {
		t.Errorf("recovered slug = %q, want loan-application", notifier.Slug)
	}
	if notifier.Flow.Trigger != capture.TriggerEvent {
		t.Errorf("notifier trigger = %q, want event", notifier.Flow.Trigger)
	}
	tr, err := canon.Canonicalize(notifier.Flow, nil)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if got := BoundaryEffects(tr.Root); !equalStrs(got, []string{"CONSUME loan.approved", "HTTP POST email-gw /send"}) {
		t.Errorf("notifier boundary effects = %v, want [CONSUME loan.approved, HTTP POST email-gw /send]", got)
	}

	// WholeFlows stitches both traces into one connected tree for rendering.
	wf := WholeFlows(spans)
	if len(wf) != 1 {
		t.Fatalf("WholeFlows: got %d, want 1 stitched tree", len(wf))
	}
	if wf[0].Synthesized {
		t.Errorf("the link should connect the consumer to the producer; root must not be synthesized")
	}
	if len(wf[0].Flow.Spans) != 4 {
		t.Errorf("stitched whole flow should keep all 4 spans, got %d", len(wf[0].Flow.Spans))
	}
	// The async hand-off survives canonicalization as an Async edge, so the
	// cross-service diagram can render the consumer as a distinct asynchronous hop.
	wtr, err := canon.Canonicalize(wf[0].Flow, nil)
	if err != nil {
		t.Fatalf("canonicalize whole flow: %v", err)
	}
	if !hasAsyncSpan(wtr.Root) {
		t.Errorf("link-stitched consumer should canonicalize to an Async span:\n%s", marshalTrace(t, wtr))
	}
}

func hasAsyncSpan(s *ir.CanonicalSpan) bool {
	if s == nil {
		return false
	}
	if s.Async {
		return true
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			if hasAsyncSpan(m) {
				return true
			}
		}
	}
	return false
}

// TestIngestAWSSNSSQSFixture is the end-to-end AWS path: SNS/SQS are emitted by
// the AWS SDK as CLIENT-kind RPC spans carrying messaging.* attributes. Decoding
// + canonicalizing must recover the broker interactions from those attributes —
// PUBLISH the SNS topic, CONSUME the SQS queue, and SETTLE (the delete that
// drains the message) as a distinct effect from the receive — rather than three
// bare HTTP/RPC calls. Sequential disjoint timing must also render them as
// ordered steps, not concurrent.
func TestIngestAWSSNSSQSFixture(t *testing.T) {
	spans, err := otlpjson.DecodeFile("../../testdata/otlp/aws-sns-sqs.otlp.json")
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
	got := BoundaryEffects(tr.Root)
	want := []string{
		"CONSUME loan-queue",
		"HTTP POST /loan-application",
		"PUBLISH loan-events",
		"SETTLE loan-queue",
	}
	if !equalStrs(got, want) {
		t.Errorf("AWS boundary effects = %v\n                   want %v", got, want)
	}

	// The three broker ops have disjoint intervals well past the order guard, so
	// they must render as three sequential steps in publish→receive→settle order,
	// not as one concurrent group.
	if n := len(tr.Root.Children); n != 3 {
		t.Fatalf("want 3 sequential steps under the entry, got %d", n)
	}
	order := []string{"PUBLISH loan-events", "CONSUME loan-queue", "SETTLE loan-queue"}
	for i, w := range order {
		g := tr.Root.Children[i]
		if g.Concurrent || g.Unordered {
			t.Errorf("step %d should be a plain sequential step, got %+v", i, g)
		}
		if g.Members[0].Op != w {
			t.Errorf("step %d = %q, want %q", i, g.Members[0].Op, w)
		}
	}
}

// TestIngestKoalafiEventBridgeShape reproduces the real Koalafi telemetry: four
// awaited steps captured as four separate root traces (no cross-trace parent),
// disjoint by ~1.1s, with the AWS-SDK attribute shape (CLIENT-kind RPC spans;
// SNS carries messaging.*, SQS carries only rpc.method + messaging.system, and
// both carry HTTP-to-LocalStack transport attrs). It pins the three flowmap-side
// defects the telemetry exposed:
//
//	#1 cross-trace ordering — the four disjoint root traces must sequence by their
//	   root intervals (publish→receive→delete→drain), not collapse into one
//	   op-key-ordered concurrent group.
//	#2 SNS publish — recovered as PUBLISH <topic> from messaging.destination.name,
//	   not lost to the bare HTTP-to-LocalStack transport.
//	#3 SQS receive vs delete — distinct (from rpc.method), not merged; and keyed to
//	   the AWS service peer (SQS), not the transport host (floci).
func TestIngestKoalafiEventBridgeShape(t *testing.T) {
	spans, err := otlpjson.DecodeFile("../../testdata/otlp/aws-eventbridge.otlp.json")
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

	// #2 + #3: the boundary-effect set recovers the publish and two distinct SQS
	// ops — none lost to bare HTTP, receive and delete not merged. The per-run event
	// id baked into the SNS topic is templated to {id} so the effect is run-stable.
	got := BoundaryEffects(tr.Root)
	want := []string{
		"PUBLISH eb-dev-evt-{id}-v1",
		"RPC SQS/DeleteMessage",
		"RPC SQS/ReceiveMessage",
	}
	if !equalStrs(got, want) {
		t.Errorf("boundary effects = %v\n                want %v", got, want)
	}

	// #1: four disjoint root traces sequence in time order, not one concurrent
	// group. The fourth step (the drain re-check) is the same op as the receive
	// but a distinct, later step — preserved, not deduped away.
	if n := len(tr.Root.Children); n != 4 {
		t.Fatalf("want 4 sequential steps (one per disjoint trace), got %d:\n%s", n, marshalTrace(t, tr))
	}
	order := []string{
		"PUBLISH eb-dev-evt-{id}-v1",
		"RPC SQS/ReceiveMessage",
		"RPC SQS/DeleteMessage",
		"RPC SQS/ReceiveMessage",
	}
	for i, w := range order {
		g := tr.Root.Children[i]
		if g.Concurrent || g.Unordered {
			t.Errorf("step %d should be a plain sequential step, got concurrent=%v unordered=%v", i, g.Concurrent, g.Unordered)
		}
		if g.Members[0].Op != w {
			t.Errorf("step %d = %q, want %q (time order)", i, g.Members[0].Op, w)
		}
	}
}

func marshalTrace(t *testing.T, tr *ir.CanonicalTrace) string {
	t.Helper()
	b, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStitchSelfLinkDoesNotDropSpan: a tagged root that links to itself must not
// be reparented onto itself — that would close a cycle and silently drop it.
func TestStitchSelfLinkDoesNotDropSpan(t *testing.T) {
	spans := []capture.Span{
		{TraceID: "A", ID: "s1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "f", serviceKey: "svc", "http.request.method": "POST", "http.route": "/x"},
			Links: []capture.SpanLink{{TraceID: "A", SpanID: "s1"}}},
	}
	tr, err := canon.Canonicalize(WholeFlows(spans)[0].Flow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Root == nil || tr.Root.Op != "HTTP POST /x" {
		t.Fatalf("self-linked root was dropped/altered; root=%+v", tr.Root)
	}
}

// TestStitchMutualLinkKeepsBothSpans: two roots that link to each other must not
// form a cycle that orphans both — the second edge is rejected, the first kept.
func TestStitchMutualLinkKeepsBothSpans(t *testing.T) {
	spans := []capture.Span{
		{TraceID: "A", ID: "a1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "f", serviceKey: "svc-a", "http.request.method": "POST", "http.route": "/a"},
			Links: []capture.SpanLink{{TraceID: "B", SpanID: "b1"}}},
		{TraceID: "B", ID: "b1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "f", serviceKey: "svc-b", "http.request.method": "POST", "http.route": "/b"},
			Links: []capture.SpanLink{{TraceID: "A", SpanID: "a1"}}},
	}
	tr, err := canon.Canonicalize(WholeFlows(spans)[0].Flow, nil)
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	collect(tr.Root, ops)
	for _, w := range []string{"HTTP POST /a", "HTTP POST /b"} {
		if !ops[w] {
			t.Errorf("mutual-link dropped span %q; ops=%v", w, keys(ops))
		}
	}
}

// TestStitchPrefersTaggedProducerLink: a new-root consumer linking to both an
// incidental untagged span and the tagged producer must reparent onto the
// producer (the membership-bearing link), not whichever resolves first.
func TestStitchPrefersTaggedProducerLink(t *testing.T) {
	spans := []capture.Span{
		{TraceID: "P", ID: "p1", Kind: ir.KindServer,
			Attrs: map[string]string{FlowKey: "f", serviceKey: "prod", "http.request.method": "POST", "http.route": "/p"}},
		{TraceID: "X", ID: "x1", Kind: ir.KindServer, // untagged, incidental
			Attrs: map[string]string{serviceKey: "other", "http.request.method": "GET", "http.route": "/x"}},
		{TraceID: "B", ID: "b1", Kind: ir.KindConsumer,
			Attrs: map[string]string{serviceKey: "cons", "messaging.destination.name": "q"},
			Links: []capture.SpanLink{{TraceID: "X", SpanID: "x1"}, {TraceID: "P", SpanID: "p1"}}},
	}
	g := Group(spans)
	var cons *FlowCapture
	for i := range g {
		if g[i].Service == "cons" {
			cons = &g[i]
		}
	}
	if cons == nil || cons.Slug != "f" {
		t.Fatalf("consumer did not join flow via the tagged producer link; services=%v", services(g))
	}
}

// TestAssembleRecognizesAWSConsumerEntry: an AWS-SDK consumer roots its own trace
// as a CLIENT span with messaging consume attributes; it must be recognized as a
// consumer entry (TriggerEvent, no synthesis) via its effective kind.
func TestAssembleRecognizesAWSConsumerEntry(t *testing.T) {
	spans := []capture.Span{
		{TraceID: "B", ID: "c1", Kind: ir.KindClient,
			Attrs: map[string]string{FlowKey: "f", serviceKey: "notifier",
				"messaging.system": "aws_sqs", "messaging.destination.name": "q", "messaging.operation": "receive"}},
	}
	g := Group(spans)
	if len(g) != 1 {
		t.Fatalf("got %d fragments, want 1", len(g))
	}
	if g[0].Synthesized {
		t.Error("AWS consumer entry (CLIENT span) should not be synthesized")
	}
	if g[0].Flow.Trigger != capture.TriggerEvent {
		t.Errorf("trigger = %q, want event", g[0].Flow.Trigger)
	}
}

func services(g []FlowCapture) []string {
	out := make([]string, len(g))
	for i := range g {
		out[i] = g[i].Service
	}
	return out
}

// TestWholeFlowsSynthesizedUsesCommonService: a synthesized root (no inbound
// entry) whose flow belongs entirely to one service names its lifeline after
// that service — the unambiguous owner — not the flow slug.
func TestWholeFlowsSynthesizedUsesCommonService(t *testing.T) {
	spans := []capture.Span{
		span("1", "remote", "emit", "emitter", ir.KindProducer, map[string]string{"messaging.destination.name": "x"}),
		span("2", "remote", "emit", "emitter", ir.KindProducer, map[string]string{"messaging.destination.name": "y"}),
	}
	wf := WholeFlows(spans)
	if len(wf) != 1 {
		t.Fatalf("got %d, want 1", len(wf))
	}
	if !wf[0].Synthesized {
		t.Fatal("two parentless producers should synthesize a root")
	}
	if wf[0].Service != "emitter" || wf[0].Flow.Service != "emitter" {
		t.Errorf("synthesized whole-flow Service = %q/%q, want owning service \"emitter\"", wf[0].Service, wf[0].Flow.Service)
	}
	// The synthesized span's own service stays empty — the label is render-only and
	// must not leak into the per-span model the system-context graph keys on.
	if wf[0].Flow.Root.Attr("service.name") != "" {
		t.Errorf("synthesized root span should carry no service.name, got %q", wf[0].Flow.Root.Attr("service.name"))
	}
}

// TestWholeFlowsSynthesizedMultiServiceFallsBackToSlug: when a synthesized whole
// flow spans more than one service there is no single owner, so the lifeline
// falls back to the flow slug rather than picking one service arbitrarily or
// rendering an unnamed participant.
func TestWholeFlowsSynthesizedMultiServiceFallsBackToSlug(t *testing.T) {
	spans := []capture.Span{
		span("1", "remote", "emit", "svc-a", ir.KindProducer, map[string]string{"messaging.destination.name": "x"}),
		span("2", "remote", "emit", "svc-b", ir.KindProducer, map[string]string{"messaging.destination.name": "y"}),
	}
	wf := WholeFlows(spans)
	if len(wf) != 1 {
		t.Fatalf("got %d, want 1", len(wf))
	}
	if !wf[0].Synthesized {
		t.Fatal("two parentless producers should synthesize a root")
	}
	if wf[0].Service != "emit" {
		t.Errorf("multi-service synthesized flow Service = %q, want slug \"emit\"", wf[0].Service)
	}
}
