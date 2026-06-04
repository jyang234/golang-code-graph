package render

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/irtest"
	"github.com/jyang234/golang-code-graph/internal/syscontext"
	"github.com/jyang234/golang-code-graph/ir"
)

func span(op string, kind ir.Kind, peer string, kids ...ir.ChildGroup) *ir.CanonicalSpan {
	return irtest.Span(op, kind, peer, kids...)
}
func seq(members ...*ir.CanonicalSpan) ir.ChildGroup  { return irtest.Seq(members...) }
func conc(members ...*ir.CanonicalSpan) ir.ChildGroup { return irtest.Conc(members...) }

// fixtureIR is the post-canon loan-application shape: a concurrent DB-read ∥
// credit-bureau pair, then sequential charge, publish, and a self-internal audit.
func fixtureIR() *ir.CanonicalTrace {
	return &ir.CanonicalTrace{
		Flow:    "POST /loan-application",
		Service: "loansvc",
		Root: span("HTTP POST /loan-application", ir.KindServer, "",
			conc(
				span("DB postgresql SELECT applicants", ir.KindClient, "postgresql"),
				span("HTTP GET credit-bureau /score/{id}", ir.KindClient, "credit-bureau"),
			),
			seq(span("HTTP POST payment-gw /charge/{id}", ir.KindClient, "payment-gw")),
			seq(span("PUBLISH loan.approved", ir.KindProducer, "Bus")),
			seq(span("auditLog", ir.KindInternal, "")),
		),
	}
}

func TestMermaidStructure(t *testing.T) {
	out := Mermaid(fixtureIR())

	if !strings.HasPrefix(out, "sequenceDiagram\n") {
		t.Fatalf("missing header:\n%s", out)
	}
	mustContain(t, out, "par concurrent")
	mustContain(t, out, "\n    and\n")
	mustContain(t, out, "end")
	mustContain(t, out, "DB postgresql SELECT applicants")
	mustContain(t, out, "HTTP GET credit-bureau /score/{id}")
	mustContain(t, out, "PUBLISH loan.approved")
	// The IR has no branches, so the renderer must never emit alt.
	if strings.Contains(out, "alt") {
		t.Errorf("renderer emitted alt (forbidden):\n%s", out)
	}
}

func TestMermaidParticipantsDeterministicOrder(t *testing.T) {
	out := Mermaid(fixtureIR())
	// Caller first, then self, then peers sorted alphabetically.
	order := []string{
		"participant Client as Client",
		"participant loansvc as loansvc",
		"participant Bus as Bus",
		"participant credit_bureau as credit-bureau",
		"participant payment_gw as payment-gw",
		"participant postgresql as postgresql",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i < 0 {
			t.Fatalf("missing participant %q in:\n%s", want, out)
		}
		if i < last {
			t.Errorf("participant %q out of order:\n%s", want, out)
		}
		last = i
	}
}

func TestMermaidDeterministic(t *testing.T) {
	a := Mermaid(fixtureIR())
	b := Mermaid(fixtureIR())
	if a != b {
		t.Error("render is not deterministic")
	}
}

func TestMermaidConsumerRootCaller(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Flow:    "consume payment.settled",
		Service: "loansvc",
		Root: span("CONSUME payment.settled", ir.KindConsumer, "Bus",
			seq(span("DB postgres UPDATE loans", ir.KindClient, "postgres")),
		),
	}
	out := Mermaid(tr)
	mustContain(t, out, "Bus->>loansvc: CONSUME payment.settled")
}

func TestMermaidMultiplicityNote(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "s",
		Root: span("HTTP POST /batch", ir.KindServer, "",
			ir.ChildGroup{Multiplicity: "1..*", Members: []*ir.CanonicalSpan{
				span("DB postgres INSERT items", ir.KindClient, "postgres"),
			}},
		),
	}
	out := Mermaid(tr)
	mustContain(t, out, "Note over s: ×1..*")
}

func TestMermaidErrorAnnotation(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "s",
		Root: span("HTTP POST /x", ir.KindServer, "",
			seq(&ir.CanonicalSpan{Op: "HTTP POST payment-gw /charge/{id}", Kind: ir.KindClient, Peer: "payment-gw", Status: "error", ErrorType: "timeout"}),
		),
	}
	out := Mermaid(tr)
	mustContain(t, out, "HTTP POST payment-gw /charge/{id} [timeout]")
}

func mustContain(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("output missing %q:\n%s", want, out)
	}
}

// TestSystemMermaidCrossService: the whole-flow renderer switches lifelines per
// span's owning Service, draws the cross-service hops, and collapses a callee's
// own entry span (no redundant self-hop).
func TestSystemMermaidCrossService(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "loansvc",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /loan-application", Kind: ir.KindServer, Service: "loansvc",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "HTTP GET credit-bureau /score/{id}", Kind: ir.KindClient, Peer: "credit-bureau", Service: "loansvc",
					Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
						{Op: "HTTP GET /score", Kind: ir.KindServer, Service: "credit-bureau",
							Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
								{Op: "DB postgres SELECT bureau", Kind: ir.KindClient, Peer: "postgres", Service: "credit-bureau"},
							}}}},
					}}}},
			}}},
		},
	}
	out := SystemMermaid(tr)
	for _, want := range []string{
		"Client->>loansvc: HTTP POST /loan-application",
		"loansvc->>credit_bureau: HTTP GET credit-bureau /score/{id}",
		"credit_bureau->>postgres: DB postgres SELECT bureau",
		"participant loansvc", "participant credit_bureau", "participant postgres",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "credit_bureau->>credit_bureau") {
		t.Errorf("callee entry span drew a redundant self-hop:\n%s", out)
	}
}

// TestSystemMermaidInternalDBNotDrawn: an ORM-emitted DB op arrives as
// KindInternal with the db system as its peer and lands on its own service. The
// system view must NOT draw a hop to the database — that self-landing reach is
// reserved for consumer polls (KindConsumer) — matching serviceInfra, which counts a
// DB participant only where the hop lands on the peer.
func TestSystemMermaidInternalDBNotDrawn(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "loansvc",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /x", Kind: ir.KindServer, Service: "loansvc",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgres INSERT ledger", Kind: ir.KindInternal, Peer: "postgres", Service: "loansvc"},
			}}},
		},
	}
	if out := SystemMermaid(tr); strings.Contains(out, "postgres") {
		t.Errorf("internal-kind DB op drew a phantom DB hop/participant:\n%s", out)
	}
}

// TestSystemMermaidConsumerPollDrawn: the intended case still works — a nested
// KindConsumer poll that lands on its own service draws its reach to the bus.
func TestSystemMermaidConsumerPollDrawn(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "loansvc",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /x", Kind: ir.KindServer, Service: "loansvc",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "CONSUME q", Kind: ir.KindConsumer, Peer: "Bus", Service: "loansvc"},
			}}},
		},
	}
	if out := SystemMermaid(tr); !strings.Contains(out, "loansvc->>Bus: CONSUME q") {
		t.Errorf("consumer poll should draw a reach to the bus:\n%s", out)
	}
}

// TestSystemMermaidRootedAt centers the view on a middle service: the caller is
// the real upstream, the subtree is the service's downstream, and ops above the
// service are excluded. An absent service returns ok=false.
func TestSystemMermaidRootedAt(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "loansvc",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /loan-application", Kind: ir.KindServer, Service: "loansvc",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "HTTP GET credit-bureau /score/{id}", Kind: ir.KindClient, Peer: "credit-bureau", Service: "loansvc",
					Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
						{Op: "HTTP GET /score", Kind: ir.KindServer, Service: "credit-bureau",
							Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
								{Op: "DB postgres SELECT bureau", Kind: ir.KindClient, Peer: "postgres", Service: "credit-bureau"},
							}}}},
					}}}},
			}}},
		},
	}
	out, ok := SystemMermaidRootedAt(tr, "credit-bureau")
	if !ok {
		t.Fatal("credit-bureau should be found")
	}
	if !strings.Contains(out, "loansvc->>credit_bureau: HTTP GET /score") {
		t.Errorf("expected upstream→svc entry arrow, got:\n%s", out)
	}
	if !strings.Contains(out, "credit_bureau->>postgres: DB postgres SELECT bureau") {
		t.Errorf("expected the service's subtree, got:\n%s", out)
	}
	if strings.Contains(out, "/loan-application") {
		t.Errorf("a rooted view must exclude ops above the service:\n%s", out)
	}
	if _, ok := SystemMermaidRootedAt(tr, "absent"); ok {
		t.Error("absent service should return ok=false")
	}
}

// TestSystemMermaidAsyncLinkDistinct: a consumer reached across a broker by a
// FOLLOWS_FROM span link (Async) renders as a distinct asynchronous interaction —
// a dashed open arrow and a Note — pulled out of the producer's synchronous par,
// not as a solid call nested inside it.
func TestSystemMermaidAsyncLinkDistinct(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "event-bus",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /publish", Kind: ir.KindServer, Service: "event-bus",
			Children: []ir.ChildGroup{{Concurrent: true, Members: []*ir.CanonicalSpan{
				{Op: "DB postgresql SELECT publishers", Kind: ir.KindClient, Peer: "postgresql/event_bus_test", Service: "event-bus"},
				{Op: "PUBLISH cgate-email", Kind: ir.KindProducer, Peer: "Bus", Service: "event-bus"},
				// The consumer was stitched onto the producer's sibling group via a link.
				{Op: "CONSUME cgate-email", Kind: ir.KindConsumer, Peer: "Bus", Service: "cgate", Async: true,
					Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
						{Op: "DB postgresql INSERT messages", Kind: ir.KindClient, Peer: "postgresql/cgate_test", Service: "cgate"},
					}}}},
			}}},
		},
	}
	out := SystemMermaid(tr)
	// The async hop is a dashed open arrow with an async note, not a solid arrow.
	mustContain(t, out, "--)cgate: CONSUME cgate-email")
	mustContain(t, out, "Note over cgate: async (FOLLOWS_FROM)")
	if strings.Contains(out, "->>cgate: CONSUME cgate-email") {
		t.Errorf("async consumer drawn as a synchronous call:\n%s", out)
	}
	// The split databases are distinct participants.
	mustContain(t, out, "participant postgresql_event_bus_test as postgresql/event_bus_test")
	mustContain(t, out, "participant postgresql_cgate_test as postgresql/cgate_test")
	// cgate's own downstream still renders.
	mustContain(t, out, "cgate->>postgresql_cgate_test: DB postgresql INSERT messages")
	// The consumer was not left inside the producer's par as a third sync branch:
	// the par block should hold only the two synchronous siblings.
	if strings.Count(out, "\n    and\n") != 1 {
		t.Errorf("expected the par to hold exactly two sync members (one `and`):\n%s", out)
	}
}

// TestSystemMermaidBoxesOwnedInfra: each service is boxed with the databases it
// exclusively owns, so per-service infrastructure stays family-adjacent instead of
// interleaving alphabetically. A shared broker stays outside any box.
func TestSystemMermaidBoxesOwnedInfra(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "event-bus",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /publish", Kind: ir.KindServer, Service: "event-bus",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgresql SELECT publishers", Kind: ir.KindClient, Peer: "postgresql/event_bus_test", Service: "event-bus"},
				{Op: "PUBLISH cgate-email", Kind: ir.KindProducer, Peer: "Bus", Service: "event-bus"}, // draws event-bus→Bus
				{Op: "CONSUME cgate-email", Kind: ir.KindConsumer, Peer: "Bus", Service: "cgate", Async: true,
					Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
						{Op: "DB postgresql INSERT messages", Kind: ir.KindClient, Peer: "postgresql/cgate_test", Service: "cgate"},
					}}}},
			}}},
		},
	}
	out := SystemMermaid(tr)
	// Each service's owned database sits inside that service's box.
	for _, want := range []string{
		"box transparent event-bus\n    participant event_bus as event-bus\n    participant postgresql_event_bus_test as postgresql/event_bus_test\n    end\n",
		"box transparent cgate\n    participant cgate as cgate\n    participant postgresql_cgate_test as postgresql/cgate_test\n    end\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing boxed family layout:\nwant:\n%s\ngot:\n%s", want, out)
		}
	}
	// The shared broker is declared outside any box (after the service boxes).
	if i, j := strings.Index(out, "end\n    participant Bus as Bus"), strings.Index(out, "box"); i < 0 || i < j {
		t.Errorf("shared Bus should be declared unboxed after the service boxes:\n%s", out)
	}
}

// TestSystemMermaidPrunesEdgelessParticipants: a lifeline that no arrow or note
// touches (here a Bus that a nested consumer references as its peer but is never
// drawn to, because the consumer hop lands on the consuming service) is pruned, not
// declared as a bare, dangling participant.
func TestSystemMermaidPrunesEdgelessParticipants(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "publisher",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /x", Kind: ir.KindServer, Service: "publisher",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				// A link-stitched consumer: its hop draws publisher→notifier, so its
				// Peer "Bus" is referenced by no edge.
				{Op: "CONSUME e", Kind: ir.KindConsumer, Peer: "Bus", Service: "notifier", Async: true},
			}}},
		},
	}
	out := SystemMermaid(tr)
	mustContain(t, out, "publisher--)notifier: CONSUME e")
	if strings.Contains(out, "participant Bus") {
		t.Errorf("edgeless Bus participant should be pruned:\n%s", out)
	}
	// Every declared participant must appear on some arrow.
	for _, p := range []string{"publisher", "notifier"} {
		if !strings.Contains(out, "participant "+p+" as "+p) {
			t.Errorf("expected participant %q declared:\n%s", p, out)
		}
	}
}

// TestSystemMermaidBoxOwnerMatchesDrawnArrow: a database is boxed under the lifeline
// the DB hop is actually drawn from, never under the span's service.name when the two
// differ (a DB nested under a same-service client call, where the hop is drawn from
// the call's peer). The box must not claim a database that service never visibly
// reaches.
func TestSystemMermaidBoxOwnerMatchesDrawnArrow(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "A",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /x", Kind: ir.KindServer, Service: "A",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "HTTP GET B /v", Kind: ir.KindClient, Peer: "B", Service: "A",
					Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
						{Op: "DB postgres SELECT t", Kind: ir.KindClient, Peer: "db1", Service: "A"},
					}}}},
			}}},
		},
	}
	out := SystemMermaid(tr)
	mustContain(t, out, "B->>db1: DB postgres SELECT t")
	if strings.Contains(out, "box transparent A") {
		t.Errorf("db1 is reached from B, not A; it must not be boxed under A:\n%s", out)
	}
}

// TestSystemMermaidNestedProducerVisible: a publish nested under another publish is
// still issued by the service, so it must render as a service→bus hop, not vanish as
// a suppressed bus→bus self-hop (the reciprocal of the consumer self-landing rule —
// a producer's children are threaded from its own service, not the bus it landed on).
func TestSystemMermaidNestedProducerVisible(t *testing.T) {
	inner := &ir.CanonicalSpan{Op: "PUBLISH inner", Kind: ir.KindProducer, Peer: "Bus", Service: "svc"}
	outer := &ir.CanonicalSpan{Op: "PUBLISH outer", Kind: ir.KindProducer, Peer: "Bus", Service: "svc",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{inner}}}}
	tr := &ir.CanonicalTrace{Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer, Service: "svc",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{outer}}}}}
	out := SystemMermaid(tr)
	mustContain(t, out, "svc->>Bus: PUBLISH outer")
	mustContain(t, out, "svc->>Bus: PUBLISH inner")
}

// TestSystemMermaidAsyncConsumerFromPublisher: when a stitched async consumer is
// nested under the producer it followed (the real reparented shape), its dashed hop
// is drawn from the publishing service — "caused by this publish" — not from the bus.
func TestSystemMermaidAsyncConsumerFromPublisher(t *testing.T) {
	cons := &ir.CanonicalSpan{Op: "CONSUME evt", Kind: ir.KindConsumer, Peer: "Bus", Service: "Y", Async: true}
	prod := &ir.CanonicalSpan{Op: "PUBLISH evt", Kind: ir.KindProducer, Peer: "Bus", Service: "X",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{cons}}}}
	tr := &ir.CanonicalTrace{Service: "X", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /p", Kind: ir.KindServer, Service: "X",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{prod}}}}}
	out := SystemMermaid(tr)
	mustContain(t, out, "X->>Bus: PUBLISH evt")
	mustContain(t, out, "X--)Y: CONSUME evt")
	if strings.Contains(out, "Bus--)Y") {
		t.Errorf("async consumer should be drawn from the publishing service, not the bus:\n%s", out)
	}
}

// TestSystemMermaidProducerRootChildrenFromService: when the root is a producer
// (reachable via SystemMermaidRootedAt on a producer-entry), its children are issued
// by the producer's service, so the body draws them from that service — matching the
// childFrom seed serviceInfra walks from. The DB child must read Y->db1 (boxed under
// Y), not Bus->db1, so box ownership and the drawn arrow agree.
func TestSystemMermaidProducerRootChildrenFromService(t *testing.T) {
	db := &ir.CanonicalSpan{Op: "DB postgres SELECT t", Kind: ir.KindClient, Peer: "db1", Service: "Y"}
	root := &ir.CanonicalSpan{Op: "PUBLISH evt", Kind: ir.KindProducer, Peer: "Bus", Service: "Y",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{db}}}}
	out := SystemMermaid(&ir.CanonicalTrace{Service: "Y", Root: root})
	mustContain(t, out, "Y->>db1: DB postgres SELECT t")
	mustContain(t, out, "box transparent Y\n    participant Y as Y\n    participant db1 as db1\n    end\n")
	if strings.Contains(out, "Bus->>db1") {
		t.Errorf("producer root's DB child must be drawn from its service, not the bus:\n%s", out)
	}
}

// TestSystemMermaidSynthRootDrawsFromClient: a flow with no single inbound entry
// (ingest synthesizes an internal root named after the flow slug) renders its entry
// points from "Client", not from the slug — the synthetic root is the external caller,
// not a participant.
func TestSystemMermaidSynthRootDrawsFromClient(t *testing.T) {
	e1 := &ir.CanonicalSpan{Op: "HTTP PUT /v1/x", Kind: ir.KindServer, Service: "golang_test_app"}
	e2 := &ir.CanonicalSpan{Op: "HTTP POST /v1/y", Kind: ir.KindServer, Service: "cgate"}
	root := &ir.CanonicalSpan{Op: "delivery_roundtrip", Kind: ir.KindInternal, // Service "" => synthesized
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{e1, e2}}}}
	out := SystemMermaid(&ir.CanonicalTrace{Service: "delivery_roundtrip", Root: root})
	mustContain(t, out, "Client->>golang_test_app: HTTP PUT /v1/x")
	mustContain(t, out, "Client->>cgate: HTTP POST /v1/y")
	if strings.Contains(out, "delivery_roundtrip") {
		t.Errorf("the flow slug must not render as a participant or caller:\n%s", out)
	}
}

// TestSystemMermaidBrokerMergesOntoServiceParticipant: when a producer's broker peer
// coincides with a real service in the flow (messaging.system canonicalizes to a name
// equal to a service.name — the clean/symmetric instrumentation case), the publish is
// drawn to that service participant, not a synthetic duplicate "Bus" node.
func TestSystemMermaidBrokerMergesOntoServiceParticipant(t *testing.T) {
	prod := &ir.CanonicalSpan{Op: "PUBLISH cgate-email", Kind: ir.KindProducer, Peer: "event_bus", Service: "app", Async: true}
	ebServer := &ir.CanonicalSpan{Op: "HTTP PUT /v1/events/{id}", Kind: ir.KindServer, Service: "event_bus"}
	root := &ir.CanonicalSpan{Op: "HTTP POST /publish", Kind: ir.KindServer, Service: "app",
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{prod, ebServer}}}}
	out := SystemMermaid(&ir.CanonicalTrace{Service: "app", Root: root})
	mustContain(t, out, "app--)event_bus: PUBLISH cgate-email")
	if strings.Contains(out, "participant Bus") {
		t.Errorf("a named broker that is a real service must not fabricate a synthetic Bus participant:\n%s", out)
	}
}

// TestSystemMermaidConsumerRootUsesBrokerPeer: a consumer-rooted flow whose broker
// is a managed system (Peer canonicalized to "SQS"/"SNS") draws the trigger arrow
// from that broker and declares it exactly once — not from a hardcoded, dangling
// "Bus" lifeline.
func TestSystemMermaidConsumerRootUsesBrokerPeer(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "notifier",
		Root: &ir.CanonicalSpan{
			Op: "CONSUME loan-queue", Kind: ir.KindConsumer, Peer: "SQS", Service: "notifier",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgres UPDATE loans", Kind: ir.KindClient, Peer: "postgres", Service: "notifier"},
			}}},
		},
	}
	out := SystemMermaid(tr)
	mustContain(t, out, "SQS->>notifier: CONSUME loan-queue")
	if strings.Contains(out, "Bus") {
		t.Errorf("consumer root from a managed broker must not draw a phantom Bus lifeline:\n%s", out)
	}
	if n := strings.Count(out, "participant SQS as SQS"); n != 1 {
		t.Errorf("broker SQS should be declared exactly once (caller == consumer peer), got %d:\n%s", n, out)
	}

	// The default event bus (no managed system) keeps the generic "Bus" caller, so
	// existing single-bus consumer diagrams are unchanged.
	busTr := &ir.CanonicalTrace{
		Service: "loansvc",
		Root:    &ir.CanonicalSpan{Op: "CONSUME payment.settled", Kind: ir.KindConsumer, Peer: "Bus", Service: "loansvc"},
	}
	if out := SystemMermaid(busTr); !strings.Contains(out, "Bus->>loansvc: CONSUME payment.settled") {
		t.Errorf("default-bus consumer root should still be triggered from Bus:\n%s", out)
	}
}

// TestSystemMermaidAllAsyncGroupKeepsFraming: a producer fanned out to several
// link-stitched consumers (a concurrent group of async members) still renders the
// concurrency framing — a par block of dashed arrows — rather than bare hops.
func TestSystemMermaidAllAsyncGroupKeepsFraming(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "publisher",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /fanout", Kind: ir.KindServer, Service: "publisher",
			Children: []ir.ChildGroup{{Concurrent: true, Members: []*ir.CanonicalSpan{
				{Op: "CONSUME a", Kind: ir.KindConsumer, Peer: "Bus", Service: "c1", Async: true},
				{Op: "CONSUME b", Kind: ir.KindConsumer, Peer: "Bus", Service: "c2", Async: true},
			}}},
		},
	}
	out := SystemMermaid(tr)
	mustContain(t, out, "par concurrent")
	mustContain(t, out, "publisher--)c1: CONSUME a")
	mustContain(t, out, "publisher--)c2: CONSUME b")
	if strings.Count(out, "\n    and\n") != 1 { // the two async members share one par block
		t.Errorf("two concurrent async consumers should share one par block:\n%s", out)
	}
}

// TestSystemMermaidBoxTitleNotSwallowedAsColor: a service whose name is a color word
// still appears as the box title (an explicit transparent color occupies the color
// slot), not as the box's background color.
func TestSystemMermaidBoxTitleNotSwallowedAsColor(t *testing.T) {
	tr := &ir.CanonicalTrace{
		Service: "aqua",
		Root: &ir.CanonicalSpan{
			Op: "HTTP POST /x", Kind: ir.KindServer, Service: "aqua",
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgres INSERT t", Kind: ir.KindClient, Peer: "postgres", Service: "aqua"},
			}}},
		},
	}
	mustContain(t, SystemMermaid(tr), "box transparent aqua\n")
}

// TestSystemGraph renders a system-context graph: graph LR, node shapes by kind,
// solid vs dashed edges.
func TestSystemGraph(t *testing.T) {
	g := &syscontext.Graph{
		Nodes: []syscontext.Node{
			{Name: "loansvc", Kind: syscontext.KindService},
			{Name: "Bus", Kind: syscontext.KindBroker},
			{Name: "pg", Kind: syscontext.KindExternal},
		},
		Edges: []syscontext.Edge{
			{From: "loansvc", To: "Bus", Label: "publish e1", Dashed: false},
			{From: "loansvc", To: "pg", Label: "DB", Dashed: true},
		},
	}
	out := SystemGraph(g)
	for _, want := range []string{
		"graph LR",
		`Bus{{"Bus"}}`,       // broker hexagon
		`pg(["pg"])`,         // external stadium
		`loansvc["loansvc"]`, // service rectangle
		`-->|"publish e1"|`,  // solid edge
		`-.->|"DB"|`,         // dashed (contract-only) edge
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
