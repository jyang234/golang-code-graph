package graphio

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// highFanOutGraph models the addendum's case (item 3): a shared higher-order site
// (RunInTx) the analysis resolved to many closures, so VTA cannot tell them apart and
// every closure looks reachable from any route entering the site. The edge INTO the site
// (caller→RunInTx) is a real call; the edges OUT of it (and the effect beneath one) are
// the over-approximate fan-out — including a spurious DELETE the create route never runs.
func highFanOutGraph() *Graph {
	const (
		caller = "ex.com/p.Caller"
		site   = "ex.com/p.RunInTx"
		handA  = "ex.com/p.CreateHandle"
		handB  = "ex.com/p.DeleteHandle"
	)
	return &Graph{
		Algo: "vta",
		Nodes: []Node{
			{FQN: caller, Tier: 1, Package: "ex.com/p"},
			{FQN: site, Tier: 1, Package: "ex.com/p"},
			{FQN: handA, Tier: 1, Package: "ex.com/p"},
			{FQN: handB, Tier: 1, Package: "ex.com/p"},
		},
		Edges: []Edge{
			{From: caller, To: site},                           // real call INTO the site — not fan-out
			{From: site, To: handA},                            // fan-out edge
			{From: site, To: handB},                            // fan-out edge (the spurious one)
			{From: handB, To: "boundary:db DELETE events"},     // effect BELOW a non-fan-out node — NOT styled
			{From: site, To: "boundary:bus PUBLISH committed"}, // a fan-out EFFECT edge — styled
		},
		BlindSpots: []blindspots.BlindSpot{
			{Kind: blindspots.HighFanOut, Site: site, Detail: "resolves to many candidate callees"},
		},
		Entrypoints: []Entrypoint{{Kind: "http", Name: "POST /create", Fn: caller}},
	}
}

// TestFanOutEdgesStyled pins item 3: edges OUT of a HighFanOut node are styled
// amber-dashed (the over-approximation carried from the node onto the edges it governs),
// the styled set is exactly the out-edges of the site (not the edge into it, nor an
// effect below a different node), and the render discloses the fan-out in the header. The
// node-level blind-spot disclosure still coexists. Valid Mermaid throughout.
func TestFanOutEdgesStyled(t *testing.T) {
	g := highFanOutGraph()
	out := g.Mermaid(MermaidOptions{MaxTier: 2})
	assertValidMermaid(t, out)

	// Edges are emitted in g.Edges order, numbered from 0:
	//   0 caller→site (into the site — NOT fan-out)
	//   1 site→CreateHandle      (fan-out)
	//   2 site→DeleteHandle      (fan-out)
	//   3 DeleteHandle→db DELETE (effect below a non-fan-out node — NOT styled)
	//   4 site→bus PUBLISH       (fan-out effect)
	// so the styled indices are exactly 1,2,4.
	wantLink := "linkStyle 1,2,4 " + fanOutLinkStyle
	if !strings.Contains(out, wantLink) {
		t.Errorf("expected fan-out edges styled %q, got:\n%s", wantLink, out)
	}
	// The edge INTO the site must NOT be styled (index 0 absent), and the effect below
	// the non-fan-out node (index 3) must NOT be styled.
	if strings.Contains(out, "linkStyle 0") || strings.Contains(out, ",0,") || strings.Contains(out, ",3 ") || strings.Contains(out, " 0,") {
		t.Errorf("a non-fan-out edge was styled as fan-out:\n%s", out)
	}

	// The fan-out is disclosed in the header (3 fan-out edges), alongside the surviving
	// node-level blind-spot disclosure.
	if !strings.Contains(out, "3 fan-out edges drawn amber-dashed") {
		t.Errorf("expected the fan-out count disclosed in the header, got:\n%s", out)
	}
	if !strings.Contains(out, "blind .->") {
		t.Errorf("the node-level HighFanOut blind-spot disclosure must still be present:\n%s", out)
	}
}

// TestFanOutEdgesStyledRooted pins that the styling survives the ROOTED render — the view
// the addendum was written against (a create route showing a spurious path to a DELETE).
// rootedSubgraph carries the HighFanOut blind spot for the in-reach site, so its out-edges
// are styled in the per-handler diagram too.
func TestFanOutEdgesStyledRooted(t *testing.T) {
	g := highFanOutGraph()
	out, ok := g.MermaidRootedAt("POST /create", MermaidOptions{MaxTier: 2})
	if !ok {
		t.Fatal("rooted render did not resolve POST /create")
	}
	assertValidMermaid(t, out)
	if !strings.Contains(out, "linkStyle ") || !strings.Contains(out, fanOutLinkStyle) {
		t.Errorf("rooted render must style the fan-out edges out of the in-reach HighFanOut site:\n%s", out)
	}
	if !strings.Contains(out, "fan-out edge") {
		t.Errorf("rooted render must disclose the fan-out in the header:\n%s", out)
	}
}

// TestNoFanOutNoStyle pins that a graph WITHOUT a HighFanOut blind spot emits no fan-out
// linkStyle or note — the styling is strictly scoped to the disclosed over-approximation,
// so an ordinary graph's render is unchanged.
func TestNoFanOutNoStyle(t *testing.T) {
	g := highFanOutGraph()
	g.BlindSpots = nil // drop the HighFanOut disclosure
	out := g.Mermaid(MermaidOptions{MaxTier: 2})
	assertValidMermaid(t, out)
	if strings.Contains(out, fanOutLinkStyle) {
		t.Errorf("a graph with no HighFanOut spot must not emit the fan-out linkStyle:\n%s", out)
	}
	if strings.Contains(out, "fan-out edge") {
		t.Errorf("a graph with no HighFanOut spot must not disclose a fan-out:\n%s", out)
	}
}
