package main

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// triageTestGraph is a tiny graph whose one route touches a DB table, so a
// --table symptom resolves to it.
func triageTestGraph() *graph.Index {
	const handle = "example.com/svc/internal/handler.Handle"
	g := &graph.Graph{
		Nodes: []graph.Node{{FQN: handle, Sig: "func()", Tier: 1}},
		Edges: []graph.Edge{
			{From: handle, To: "boundary:db INSERT users", Boundary: "outbound-sync"},
		},
	}
	return graph.NewIndex(g)
}

// M-11: the CLI `triage` command and the MCP `triage` tool must produce the SAME
// card for the same symptom — they resolve, demand exactly one symptom, and render
// the disclosure through the ONE shared resolveTriage/renderTriage pair. This pins
// that the MCP tool's rendered body equals renderTriage of the shared resolution,
// so a future edit that re-forks either surface's rendering fails here.
func TestTriageCardParity(t *testing.T) {
	ix := triageTestGraph()

	// The shared core, as the CLI calls it.
	res, card, err := resolveTriage(ix, triageSymptom{Table: "users"})
	if err != nil {
		t.Fatalf("resolveTriage: %v", err)
	}
	cliBody := renderTriage(res, card)

	// The MCP tool, driven through call().
	srv := &mcpServer{ix: ix}
	mcpBody := toolTextOf(t, srv.call("triage", toolArgs{Table: "users"}))

	if cliBody != mcpBody {
		t.Errorf("triage card diverged between CLI and MCP:\n--- CLI ---\n%s\n--- MCP ---\n%s", cliBody, mcpBody)
	}
	if !strings.Contains(mcpBody, "example.com/svc/internal/handler.Handle") {
		t.Errorf("triage card missing the resolved suspect:\n%s", mcpBody)
	}
}

// The shared resolver fails closed identically for both surfaces: zero symptoms
// and a symptom that resolves to nothing both error.
func TestResolveTriageFailsClosed(t *testing.T) {
	ix := triageTestGraph()

	if _, _, err := resolveTriage(ix, triageSymptom{}); err == nil {
		t.Error("expected an error when no symptom is set")
	}
	if _, _, err := resolveTriage(ix, triageSymptom{Table: "users", Event: "x"}); err == nil {
		t.Error("expected an error when two symptoms are set")
	}
	if _, _, err := resolveTriage(ix, triageSymptom{Table: "nonexistent"}); err == nil {
		t.Error("expected an error when the symptom resolves to nothing")
	}
}
