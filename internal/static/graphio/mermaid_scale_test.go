package graphio

import (
	"strconv"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// bigGraph synthesizes an n-node, ~2n-edge unscoped graph with entry points, a
// boundary effect, a blind spot, and a frontier marker — so the renderers are
// exercised at a scale our fixtures never reach AND the over-cap path is tested
// WITH the disclosure channel present. Two entry points deliberately tie on
// (Name, Fn) but differ in Kind, to exercise the index sort's total-order tie-break.
func bigGraph(n int) *Graph {
	g := &Graph{Algo: "rta"}
	for i := 0; i < n; i++ {
		g.Nodes = append(g.Nodes, Node{FQN: "example.com/svc/pkg.F" + strconv.Itoa(i), Tier: 2})
	}
	for i := 0; i < n; i++ {
		g.Edges = append(g.Edges, Edge{
			From: "example.com/svc/pkg.F" + strconv.Itoa(i),
			To:   "example.com/svc/pkg.F" + strconv.Itoa((i+1)%n),
			Tier: 2,
		})
	}
	g.Edges = append(g.Edges, Edge{From: "example.com/svc/pkg.F0", To: "boundary:db INSERT t", Tier: 1, Boundary: "outbound-sync"})
	g.BlindSpots = []blindspots.BlindSpot{{Kind: blindspots.Reflect, Site: "example.com/svc/pkg.F0"}}
	g.Frontier = &FrontierSection{Markers: []frontier.Marker{
		{Kind: "dynamic-bus", Bin: frontier.BinA, Site: "bus PUBLISH <dynamic>", Owner: "example.com/svc/pkg.F1"},
	}}
	for i := 0; i < 5 && i < n; i++ {
		g.Entrypoints = append(g.Entrypoints, Entrypoint{
			Kind: "http", Name: "GET /r" + strconv.Itoa(i), Fn: "example.com/svc/pkg.F" + strconv.Itoa(i),
		})
	}
	// Same (Name, Fn), different Kind — only a total-order sort pins their order.
	g.Entrypoints = append(g.Entrypoints,
		Entrypoint{Kind: "http", Name: "GET /dup", Fn: "example.com/svc/pkg.F0"},
		Entrypoint{Kind: "consumer", Name: "GET /dup", Fn: "example.com/svc/pkg.F0"},
	)
	return g
}

func TestMermaidOverCapRendersIndex(t *testing.T) {
	g := bigGraph(500)
	out := g.Mermaid(MermaidOptions{MaxNodes: 200})
	assertValidMermaid(t, out)
	if !strings.Contains(out, "exceed the render cap (200)") {
		t.Errorf("over-cap render must disclose the cap:\n%s", clip(out))
	}
	if !strings.Contains(out, "GET /r0") {
		t.Errorf("over-cap whole-graph index must list entry points to --root at:\n%s", clip(out))
	}
	if strings.Count(out, "pkg.F") > 10 {
		t.Errorf("over-cap render must not draw the full node set")
	}
}

// TestMermaidOverCapPreservesDisclosures is the trust guard: the over-cap index must
// still draw the blind-spot and frontier disclosures, so a large graph never silently
// launders its "where the analysis goes dark" markers into clean reachability.
func TestMermaidOverCapPreservesDisclosures(t *testing.T) {
	g := bigGraph(500)
	out := g.Mermaid(MermaidOptions{MaxNodes: 200})
	if !strings.Contains(out, "⊥ reflect") {
		t.Errorf("over-cap index dropped the blind-spot disclosure (honesty channel):\n%s", clip(out))
	}
	if !strings.Contains(out, "⌖ dynamic-bus") {
		t.Errorf("over-cap index dropped the frontier disclosure:\n%s", clip(out))
	}
}

// TestMermaidCapCountsBoundaryNodes pins the fix for the under-counting cap: a graph
// whose first-party count is under the cap but whose boundary effects push the DRAWN
// node count over it must still flip to the index, not render the hairball.
func TestMermaidCapCountsBoundaryNodes(t *testing.T) {
	g := &Graph{Algo: "rta"}
	const f = 50
	for i := 0; i < f; i++ {
		fn := "example.com/s/p.F" + strconv.Itoa(i)
		g.Nodes = append(g.Nodes, Node{FQN: fn, Tier: 1})
		g.Edges = append(g.Edges, Edge{From: fn, To: "boundary:db INSERT t" + strconv.Itoa(i), Tier: 1, Boundary: "outbound-sync"})
	}
	// 50 first-party + 50 distinct boundary effects = 100 drawn; cap 80 must trip.
	out := g.Mermaid(MermaidOptions{MaxNodes: 80})
	assertValidMermaid(t, out)
	if !strings.Contains(out, "exceed the render cap (80)") {
		t.Errorf("cap must count boundary effect nodes; 100 drawn > 80 should flip to the index:\n%s", clip(out))
	}
}

func TestMermaidUnderCapRendersFull(t *testing.T) {
	g := bigGraph(50)
	out := g.Mermaid(MermaidOptions{MaxNodes: 200})
	assertValidMermaid(t, out)
	if strings.Contains(out, "exceed the render cap") {
		t.Errorf("under-cap render must draw the full graph, not the index")
	}
}

func TestMermaidCapZeroIsUncapped(t *testing.T) {
	g := bigGraph(300)
	out := g.Mermaid(MermaidOptions{MaxNodes: 0}) // library default: uncapped
	assertValidMermaid(t, out)
	if strings.Contains(out, "exceed the render cap") {
		t.Errorf("MaxNodes=0 must be uncapped")
	}
}

// TestMermaidOverCapDeterministic pins byte-identical over-cap output across repeated
// runs — the determinism guard for the new index ordering path (the entry-point sort),
// using a graph with (Name, Fn)-colliding entry points so an unstable sort would show.
func TestMermaidOverCapDeterministic(t *testing.T) {
	g := bigGraph(500)
	first := g.Mermaid(MermaidOptions{MaxNodes: 200})
	for i := 0; i < 8; i++ {
		if got := g.Mermaid(MermaidOptions{MaxNodes: 200}); got != first {
			t.Fatalf("over-cap index not deterministic on run %d", i)
		}
	}
}

func TestMermaidDiffOverCapSummarizes(t *testing.T) {
	base := bigGraph(10)
	branch := bigGraph(500)
	out := MermaidDiff(base, branch, MermaidOptions{MaxNodes: 200})
	assertValidMermaid(t, out)
	if !strings.Contains(out, "large delta") || !strings.Contains(out, "added") {
		t.Errorf("over-cap diff must summarize the delta with counts:\n%s", clip(out))
	}
}

func TestMermaidRootedOverCapSteersToNarrow(t *testing.T) {
	g := bigGraph(500)
	out, ok := g.MermaidRootedAt("GET /r0", MermaidOptions{MaxNodes: 200})
	if !ok {
		t.Fatal("GET /r0 should resolve")
	}
	assertValidMermaid(t, out)
	if !strings.Contains(out, "exceed the render cap") {
		t.Errorf("a too-large rooted reach must also disclose the cap")
	}
}

func BenchmarkMermaidLarge(b *testing.B) {
	g := bigGraph(2000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.Mermaid(MermaidOptions{MaxNodes: 0}) // uncapped: measure the real render cost
	}
}

// clip bounds a diagram to a readable prefix for failure output.
func clip(s string) string {
	if len(s) > 600 {
		return s[:600]
	}
	return s
}
