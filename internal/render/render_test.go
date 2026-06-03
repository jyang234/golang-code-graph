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
// reserved for consumer polls (KindConsumer) — matching collectSystemLifelines,
// which declares the peer participant only for consumers.
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
		"box event-bus\n    participant event_bus as event-bus\n    participant postgresql_event_bus_test as postgresql/event_bus_test\n    end\n",
		"box cgate\n    participant cgate as cgate\n    participant postgresql_cgate_test as postgresql/cgate_test\n    end\n",
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
