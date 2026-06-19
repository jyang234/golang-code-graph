package graphio

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/render"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// rootedSampleGraph is an UNSCOPED graph (Frontier populated) with two entry points
// that reach disjoint subtrees, so rooting at one must prune the other AND keep the
// rooted handler's frontier marker.
func rootedSampleGraph() *Graph {
	const (
		create  = "(*example.com/svc/internal/handler.App).Create"
		status  = "(*example.com/svc/internal/handler.App).Status"
		eval    = "(*example.com/svc/internal/origination.Evaluator).Evaluate"
		notify  = "(*example.com/svc/internal/origination.Evaluator).notify"
		selectL = "(*example.com/svc/internal/store.Loans).Select"
	)
	return &Graph{
		Algo: "rta",
		Nodes: []Node{
			{FQN: create, Tier: 1},
			{FQN: status, Tier: 1},
			{FQN: eval, Tier: 2, Fallible: true},
			{FQN: notify, Tier: 2},
			{FQN: selectL, Tier: 2, Fallible: true},
		},
		Edges: []Edge{
			{From: create, To: eval, Tier: 2},
			{From: eval, To: notify, Tier: 3},
			{From: eval, To: "boundary:bus PUBLISH loan.approved", Tier: 1, Boundary: "outbound-async"},
			{From: notify, To: "boundary:bus PUBLISH <dynamic>", Tier: 1, Boundary: "outbound-async"},
			{From: status, To: selectL, Tier: 2},
			{From: selectL, To: "boundary:db SELECT loans", Tier: 1, Boundary: "outbound-sync"},
		},
		Entrypoints: []Entrypoint{
			{Kind: "http", Name: "POST /create", Fn: create},
			{Kind: "http", Name: "GET /status", Fn: status},
		},
		Frontier: &FrontierSection{
			Markers: []frontier.Marker{
				{Kind: "dynamic-bus", Bin: frontier.BinA, Site: "bus PUBLISH <dynamic>", Owner: notify},
			},
		},
	}
}

func TestMermaidRootedKeepsFrontierAndPrunesOtherHandler(t *testing.T) {
	g := rootedSampleGraph()
	out, ok := g.MermaidRootedAt("POST /create", MermaidOptions{MaxTier: 2})
	if !ok {
		t.Fatal("expected POST /create to resolve")
	}

	// The rooted handler's frontier marker (dynamic-bus, owned by notify) survives —
	// the whole point of render-time scoping over the unscoped graph.
	if !strings.Contains(out, "⌖ dynamic-bus") {
		t.Errorf("rooted view must keep the in-scope frontier marker:\n%s", out)
	}
	// The Create subtree is shown; the disjoint Status subtree is pruned.
	if !strings.Contains(out, "origination.Evaluator.Evaluate") {
		t.Errorf("Create's subtree should be present:\n%s", out)
	}
	if strings.Contains(out, "store.Loans.Select") || strings.Contains(out, "db SELECT loans") {
		t.Errorf("the other handler's subtree must be pruned:\n%s", out)
	}
	if !strings.Contains(out, "scope: POST /create") {
		t.Errorf("header should name the scope:\n%s", out)
	}
}

func TestMermaidRootedFailsClosedOnUnknownRoot(t *testing.T) {
	g := rootedSampleGraph()
	if _, ok := g.MermaidRootedAt("DELETE /nope", MermaidOptions{MaxTier: 2}); ok {
		t.Error("an unresolved root must return ok=false, not a misleading diagram")
	}
}

// TestMermaidRootedDeterministic pins byte-identical output across repeated runs —
// the determinism guard CLAUDE.md requires for a new ordering path (the forwardReach
// BFS and the reach-based pruning), beyond the single-shot golden.
func TestMermaidRootedDeterministic(t *testing.T) {
	g := rootedSampleGraph()
	first, ok := g.MermaidRootedAt("POST /create", MermaidOptions{MaxTier: 2})
	if !ok {
		t.Fatal("POST /create should resolve")
	}
	for i := 0; i < 8; i++ {
		got, _ := g.MermaidRootedAt("POST /create", MermaidOptions{MaxTier: 2})
		if got != first {
			t.Fatalf("MermaidRootedAt not deterministic on run %d", i)
		}
	}
}

// TestMermaidRootedAmbiguousRouteFailsClosed pins the fix for the suffix-match bug:
// a route prefix that matches two distinct handlers must abstain, not resolve to
// whichever entry sorts first.
func TestMermaidRootedAmbiguousRouteFailsClosed(t *testing.T) {
	g := &Graph{
		Algo:  "rta",
		Nodes: []Node{{FQN: "a.H1", Tier: 1}, {FQN: "a.H2", Tier: 1}},
		Entrypoints: []Entrypoint{
			{Kind: "http", Name: "GET /v1/loans", Fn: "a.H1"},
			{Kind: "http", Name: "GET /v2/loans", Fn: "a.H2"},
		},
	}
	if _, ok := g.MermaidRootedAt("GET /loans", MermaidOptions{MaxTier: 2}); ok {
		t.Error("'/loans' matches both /v1/loans and /v2/loans; an ambiguous root must fail closed")
	}
	// An exact, unambiguous route still resolves.
	if _, ok := g.MermaidRootedAt("GET /v2/loans", MermaidOptions{MaxTier: 2}); !ok {
		t.Error("an exact route must still resolve")
	}
}

func TestRouteMatchesSegmentwise(t *testing.T) {
	cases := []struct {
		name, query string
		want        bool
	}{
		{"POST /loan-application", "POST /loan-application", true},
		{"/loan-application/{id}/status", "GET /loan-application/{id}/status", true},
		{"POST /loan-application", "GET /loan-application", false}, // method differs
		{"payment.settled", "payment.settled", true},
		{"POST /a", "POST /b", false},
	}
	for _, c := range cases {
		if got := routeMatches(c.name, c.query); got != c.want {
			t.Errorf("routeMatches(%q,%q)=%v want %v", c.name, c.query, got, c.want)
		}
	}
}

func TestMermaidRootedGolden(t *testing.T) {
	g := loadGraph(t, "../../../testdata/groundwork/goldens/loansvc.graph.json")
	out, ok := g.MermaidRootedAt("POST /loan-application", MermaidOptions{MaxTier: 2})
	if !ok {
		t.Fatal("POST /loan-application should resolve in the loansvc graph")
	}
	fenced := render.Fence(out)
	assertValidMermaid(t, fenced)
	assertGolden(t, "../../../testdata/groundwork/goldens/loansvc.post_loan_application.callgraph.md", fenced)
}
