package graphio

import (
	"strings"
	"testing"
)

// TestEdgeDecorationMatrix exercises every edge annotation the renderer emits —
// concurrent (go), outbound-async, and the reclaimer `via` provenance — on a single
// graph, so the reclaimed-graph path (via-tagged edges from --reclaim/--reclaim-sql)
// is covered rendering-side without a dedicated fixture.
func TestEdgeDecorationMatrix(t *testing.T) {
	g := &Graph{
		Algo: "rta",
		Nodes: []Node{
			{FQN: "x.A", Tier: 1}, {FQN: "x.B", Tier: 1}, {FQN: "x.C", Tier: 1}, {FQN: "x.D", Tier: 1},
		},
		Edges: []Edge{
			{From: "x.A", To: "x.B", Tier: 2},                                                // plain
			{From: "x.A", To: "x.C", Tier: 2, Concurrent: true},                              // go
			{From: "x.A", To: "x.D", Tier: 2, Via: "strict-server"},                          // reclaimed
			{From: "x.B", To: "boundary:bus PUBLISH e", Tier: 1, Boundary: "outbound-async"}, // async effect
			{From: "x.C", To: "boundary:db UPDATE t", Tier: 1, Boundary: "outbound-sync", Via: "sql-constfold"},
		},
	}
	out := g.Mermaid(MermaidOptions{MaxTier: 2})
	assertValidMermaid(t, out)
	for _, want := range []string{
		"x_A --> x_B",                    // plain solid
		"x_A -. go .-> x_C",              // concurrent dashed
		"x_A -->|via strict-server| x_D", // reclaimed solid with provenance
		"-. async .->",                   // async effect dashed
		"via sql-constfold",              // reclaimed effect provenance
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing edge form %q\n%s", want, out)
		}
	}
}

// TestMermaidAlgoHeaderAcrossAlgorithms confirms the substrate provenance flows into
// the header for every algorithm. Rendering itself is algo-agnostic — vta/cha change
// the GRAPH content, not the render logic — so this pins the one algo-dependent bit.
func TestMermaidAlgoHeaderAcrossAlgorithms(t *testing.T) {
	for _, algo := range []string{"rta", "vta", "cha"} {
		g := &Graph{Algo: algo, Nodes: []Node{{FQN: "x.A", Tier: 1}}}
		out := g.Mermaid(MermaidOptions{MaxTier: 2})
		assertValidMermaid(t, out)
		if !strings.Contains(out, "algo: "+algo) {
			t.Errorf("header must record algo %q:\n%s", algo, out)
		}
	}
}
