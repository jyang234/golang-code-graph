package render

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/irtest"
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
