package coverage_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/coverage"
	"github.com/jyang234/golang-code-graph/internal/irtest"
	"github.com/jyang234/golang-code-graph/internal/static/boundary"
	"github.com/jyang234/golang-code-graph/ir"
)

func sp(op string, kind ir.Kind, peer string, kids ...ir.ChildGroup) *ir.CanonicalSpan {
	return irtest.Span(op, kind, peer, kids...)
}
func seq(m ...*ir.CanonicalSpan) ir.ChildGroup        { return irtest.Seq(m...) }
func trace(root *ir.CanonicalSpan) *ir.CanonicalTrace { return irtest.Trace("loansvc", root) }

// loansvcContract mirrors the fixture's gated boundary: three published events,
// one consumed event, two external HTTP dependencies.
func loansvcContract() *boundary.Contract {
	return &boundary.Contract{
		Service: "loansvc",
		Published: []boundary.Event{
			{Event: "loan.approved", Tier: 1},
			{Event: "loan.declined", Tier: 1},
			{Event: "disbursement.initiated", Tier: 1},
		},
		Consumed: []boundary.Event{{Event: "payment.settled", Tier: 1}},
		ExternalDeps: []boundary.ExternalDep{
			{Peer: "credit-bureau", Kind: "http", Ops: []string{"GET /score/{id}"}, Tier: 1},
			{Peer: "payment-gw", Kind: "http", Ops: []string{"POST /charge/{id}"}, Tier: 1},
		},
	}
}

// happyPath is the approval flow: it exercises loan.approved, disbursement.initiated,
// and both external deps — but not the decline branch or the consumer.
func happyPath() *ir.CanonicalTrace {
	return trace(sp("HTTP POST /loan-application", ir.KindServer, "",
		seq(sp("HTTP GET credit-bureau /score/{id}", ir.KindClient, "credit-bureau")),
		seq(sp("HTTP POST payment-gw /charge/{id}", ir.KindClient, "payment-gw")),
		seq(sp("PUBLISH loan.approved", ir.KindProducer, "Bus")),
		seq(sp("PUBLISH disbursement.initiated", ir.KindProducer, "Bus")),
	))
}

func keys(r coverage.Report) []string {
	out := make([]string, len(r.Unexercised))
	for i, e := range r.Unexercised {
		out[i] = e.Key
	}
	return out
}

// TestDeltaReproducesArtifacts7 is the spec's acceptance #5: over the happy-path
// flow alone, coverage names the untested decline publish and the unconsumed
// event — and never the exercised loan.approved or the external deps.
func TestDeltaReproducesArtifacts7(t *testing.T) {
	got := keys(coverage.Delta(loansvcContract(), []*ir.CanonicalTrace{happyPath()}))
	want := []string{"CONSUME payment.settled", "PUBLISH loan.declined"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("coverage = %v, want %v", got, want)
	}
}

// TestDeltaClearsWhenFlowsAdded is the "add the decline/consumer flows → it
// clears" half: with traces covering every boundary effect, the delta is empty.
func TestDeltaClearsWhenFlowsAdded(t *testing.T) {
	decline := trace(sp("HTTP POST /loan-application", ir.KindServer, "",
		seq(sp("PUBLISH loan.declined", ir.KindProducer, "Bus")),
	))
	consumer := trace(sp("CONSUME payment.settled", ir.KindConsumer, "Bus"))

	r := coverage.Delta(loansvcContract(), []*ir.CanonicalTrace{happyPath(), decline, consumer})
	if !r.Empty() {
		t.Fatalf("expected full coverage, still unexercised: %v", keys(r))
	}
}

// TestDeltaCategorization checks each unexercised effect is tagged with its kind.
func TestDeltaCategorization(t *testing.T) {
	r := coverage.Delta(loansvcContract(), []*ir.CanonicalTrace{happyPath()})
	byKey := map[string]coverage.Category{}
	for _, e := range r.Unexercised {
		byKey[e.Key] = e.Category
	}
	if byKey["PUBLISH loan.declined"] != coverage.Publish {
		t.Errorf("loan.declined category = %q", byKey["PUBLISH loan.declined"])
	}
	if byKey["CONSUME payment.settled"] != coverage.Consume {
		t.Errorf("payment.settled category = %q", byKey["CONSUME payment.settled"])
	}
}

// TestDeltaOverCommittedFixture is the integration check: against the real
// committed boundary contract and the real committed goldens, coverage flags the
// genuinely-unexercised decline publish — and, because a payment.settled flow IS
// committed, does NOT flag that consumed event (the correct behavior; it differs
// from artifacts §7, which assumed no consumer flow).
func TestDeltaOverCommittedFixture(t *testing.T) {
	dir := fixtureDir()
	c := loadContract(t, filepath.Join(dir, ".flowmap", "boundary-contract.json"))
	traces := loadGoldens(t, filepath.Join(dir, "flows", "testdata", "flows"))
	if len(traces) < 2 {
		t.Fatalf("expected the committed goldens, found %d", len(traces))
	}

	r := coverage.Delta(c, traces)
	ks := keys(r)
	if !contains(ks, "PUBLISH loan.declined") {
		t.Errorf("expected loan.declined flagged unexercised, got %v", ks)
	}
	if contains(ks, "PUBLISH loan.approved") {
		t.Errorf("loan.approved is exercised and must not be flagged: %v", ks)
	}
	if contains(ks, "CONSUME payment.settled") {
		t.Errorf("payment.settled has a committed flow and must not be flagged: %v", ks)
	}
	if contains(ks, "HTTP GET credit-bureau /score/{id}") {
		t.Errorf("the credit-bureau dependency is exercised: %v", ks)
	}
}

func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixtures", "loansvc")
}

func loadContract(t *testing.T, path string) *boundary.Contract {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c boundary.Contract
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func loadGoldens(t *testing.T, dir string) []*ir.CanonicalTrace {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*ir.CanonicalTrace
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := ir.Load(b)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		out = append(out, tr)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
