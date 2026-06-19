package graphio

import (
	"strconv"
	"strings"
	"testing"
)

// bigGraph synthesizes an n-node, ~2n-edge unscoped graph with entry points and a
// boundary effect, to exercise the renderers at a scale our fixtures never reach.
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
	for i := 0; i < 5 && i < n; i++ {
		g.Entrypoints = append(g.Entrypoints, Entrypoint{
			Kind: "http", Name: "GET /r" + strconv.Itoa(i), Fn: "example.com/svc/pkg.F" + strconv.Itoa(i),
		})
	}
	return g
}

func TestMermaidOverCapRendersIndex(t *testing.T) {
	g := bigGraph(500)
	out := g.Mermaid(MermaidOptions{MaxNodes: 200})
	assertValidMermaid(t, out)
	if !strings.Contains(out, "exceed the render cap (200)") {
		t.Errorf("over-cap render must disclose the cap:\n%s", out[:min(len(out), 600)])
	}
	if !strings.Contains(out, "GET /r0") {
		t.Errorf("over-cap whole-graph index must list entry points to --root at:\n%s", out[:min(len(out), 600)])
	}
	// It must NOT draw the 500 nodes.
	if strings.Count(out, "pkg.F") > 10 {
		t.Errorf("over-cap render must not draw the full node set")
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

func TestMermaidDiffOverCapSummarizes(t *testing.T) {
	base := bigGraph(10)
	branch := bigGraph(500)
	out := MermaidDiff(base, branch, MermaidOptions{MaxNodes: 200})
	assertValidMermaid(t, out)
	if !strings.Contains(out, "large delta") || !strings.Contains(out, "added") {
		t.Errorf("over-cap diff must summarize the delta with counts:\n%s", out[:min(len(out), 600)])
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
