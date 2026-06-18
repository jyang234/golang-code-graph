package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/impeach"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestDeclaredBlindSpotEnactmentRoundTrip closes the §8 loop end to end across the
// trust boundary, through the REAL JSON wire: flowmap builds the impeachsvc graph
// (producer), it is marshalled and re-loaded as a groundwork graph (consumer), and
// the impeachment is audited. WITHOUT the ratified seam the impeachment fires;
// declaring it in config (the enactment a CODEOWNER commits) makes the next graph
// carry the blind spot, so groundwork abstains at the seam and the impeachment is
// REALLY extinguished — not just in the in-memory self-extinguish dry run. Because
// the proof travels the actual JSON wire, it also pins the kind-string parity: if
// flowmap's emitted kind and groundwork's blinded kind disagreed, the seam would
// not extinguish and this fails.
//
// It is an integration test: it bridges the producer (analyze/graphio) and the
// consumer (graph/impeach) that production keeps strictly separated, exercising the
// graph JSON contract between them.
func TestDeclaredBlindSpotEnactmentRoundTrip(t *testing.T) {
	const dir = "../../testdata/fixtures/impeachsvc"
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze impeachsvc: %v", err)
	}

	// Baseline: no ratified seam → both missed-root DB DELETEs (ledger + audit_log,
	// reached through the same severed route) are impeached.
	if n := auditImpeachsvc(t, buildGraphJSON(t, res)); n != 2 {
		t.Fatalf("baseline: want 2 impeachment candidates, got %d", n)
	}

	// Ratify: declare the seam the loop proposed + self-extinguish-verified. The
	// next graph build carries it as an ImpeachmentSeam blind spot.
	res.Config.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{{
		Site:   "(*example.com/impeachsvc/internal/admin.Admin).PurgeLedger",
		Reason: "ratified behavioral impeachment of db DELETE ledger on DELETE /admin/ledger",
	}}
	if n := auditImpeachsvc(t, buildGraphJSON(t, res)); n != 0 {
		t.Fatalf("after enactment: want 0 candidates (extinguished), got %d", n)
	}
}

// buildGraphJSON renders res to the graph JSON exactly as `flowmap graph` would.
func buildGraphJSON(t *testing.T, res *analyze.Result) []byte {
	t.Helper()
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	b, err := g.Marshal()
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	return b
}

// auditImpeachsvc loads graphJSON as a groundwork graph, stamps it as the gated
// commit, and audits the committed impeachsvc corpus under production provenance,
// returning the candidate count.
func auditImpeachsvc(t *testing.T, graphJSON []byte) int {
	t.Helper()
	p := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(p, graphJSON, 0o644); err != nil {
		t.Fatalf("write graph: %v", err)
	}
	g, err := graph.LoadFile(p)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	g.Stamp = "deadbeefcafe"
	ix := graph.NewIndex(g)

	traces := []*ir.CanonicalTrace{
		loadGoldenTrace(t, "../../testdata/fixtures/impeachsvc/flows/testdata/flows/delete_admin_ledger.golden.json"),
		loadGoldenTrace(t, "../../testdata/fixtures/impeachsvc/flows/testdata/flows/post_loan.golden.json"),
	}
	prov := impeach.Provenance{TraceIdentity: "deadbeefcafe", Capture: impeach.CaptureProduction}
	return len(impeach.Audit("impeachsvc", ix, traces, prov).Candidates)
}

func loadGoldenTrace(t *testing.T, path string) *ir.CanonicalTrace {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	tr, err := ir.Load(b)
	if err != nil {
		t.Fatalf("load golden: %v", err)
	}
	return tr
}
