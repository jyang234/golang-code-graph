package main

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// toolTextOf pulls the rendered text out of a tool result map.
func toolTextOf(t *testing.T, r map[string]any) string {
	t.Helper()
	content, ok := r["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tool result carries no content: %#v", r)
	}
	return content[0]["text"].(string)
}

// H-9/H-10: the MCP `fitness` tool must ride the SAME substrate/provenance
// disclosure the CLI prints — the provenance line, every caveat (substrate
// mismatch, reclaim, SQL fold), and the per-finding witness lines. Dropping them
// (as the tool used to, answering a bare "all invariants hold") laundered an
// unsound-substrate pass as a clean green run for the agent loop. GateCaveats is
// the ONE assembly both surfaces read.
func TestMCPFitnessCarriesProvenanceAndCaveats(t *testing.T) {
	const route = "example.com/svc/internal/handler.Handle"
	g := &graph.Graph{
		Algo: "rta", // graph on rta …
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			// a folded DB write label — must surface a sql-fold caveat.
			{From: route, To: "boundary:db INSERT users", Boundary: "outbound-sync", Via: "sql-constfold"},
		},
	}
	ix := graph.NewIndex(g)
	// … policy proposed on vta → a substrate mismatch caveat, plus io_budget so the
	// route's single write is judged.
	p := &policy.Policy{Service: "svc", Version: 1, Substrate: "vta", IOBudget: &policy.IOBudget{MaxWritesPerRoute: 5}}
	srv := &mcpServer{ix: ix, p: p}

	out := toolTextOf(t, srv.call("fitness", toolArgs{}))

	if !strings.Contains(out, "substrate mismatch") {
		t.Errorf("MCP fitness dropped the substrate-mismatch caveat — an unsound-substrate pass reads clean:\n%s", out)
	}
	if !strings.Contains(out, "sql-fold-informed") {
		t.Errorf("MCP fitness dropped the SQL-fold caveat (H-10):\n%s", out)
	}
	// The provenance line names the substrate the verdict was computed on.
	if !strings.Contains(out, "rta") {
		t.Errorf("MCP fitness omitted the provenance/substrate line:\n%s", out)
	}
}

// The MCP fitness text must include a caution's witness lines (the exact edge and
// the via/Detail), not just rule+summary — the same load-bearing witness the CLI
// prints via printFinding (H-9).
func TestMCPFitnessCarriesFindingWitness(t *testing.T) {
	const route = "example.com/svc/internal/handler.Handle"
	g := &graph.Graph{
		Algo:  "vta",
		Nodes: []graph.Node{{FQN: route, Sig: "func()", Tier: 1}},
		Edges: []graph.Edge{
			// One write over a budget of 0 → an io_budget violation carrying a From.
			{From: route, To: "boundary:db INSERT users", Boundary: "outbound-sync"},
		},
	}
	p := &policy.Policy{Service: "svc", Version: 1, Substrate: "vta", IOBudget: &policy.IOBudget{MaxWritesPerRoute: 0}}
	srv := &mcpServer{ix: graph.NewIndex(g), p: p}

	out := toolTextOf(t, srv.call("fitness", toolArgs{}))
	if !strings.Contains(out, "io_budget") {
		t.Fatalf("expected an io_budget violation:\n%s", out)
	}
	if !strings.Contains(out, route) {
		t.Errorf("MCP fitness dropped the per-finding witness (the From symbol):\n%s", out)
	}
}
