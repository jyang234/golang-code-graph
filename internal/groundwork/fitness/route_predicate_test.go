package fitness

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// M-9: a policy that sets io_budget but declares NO layering has an empty
// RootPackages() set. Without the CompositionRoots() fallback the enforcer would
// charge main as a route and inflate every real route's write count by main's
// startup writes. IsRoute — the ONE predicate RouteWrites, the proposers, and
// the ground card share — must exclude the authoritative composition root even
// with no layering config.
func TestIsRouteExcludesCompositionRootWithoutLayering(t *testing.T) {
	const (
		serverMain = "example.com/svc/cmd/server.main"
		handleGet  = "example.com/svc/internal/handler.HandleGet"
		migrate    = "example.com/svc/internal/store.Migrate"
	)
	g := &graph.Graph{
		CompositionRoots: []string{"example.com/svc/cmd/server"},
		Nodes: []graph.Node{
			{FQN: serverMain, Sig: "func()", Tier: 3},
			{FQN: handleGet, Sig: "func()", Tier: 1},
			{FQN: migrate, Sig: "func() error", Tier: 3},
		},
		Edges: []graph.Edge{
			// main runs startup migrations (a write) directly — not through a route.
			{From: serverMain, To: "boundary:db INSERT schema_migrations", Boundary: "db"},
			{From: serverMain, To: migrate, Tier: 3},
			{From: migrate, To: "boundary:db CREATE tables", Boundary: "db"},
			// the real route writes exactly one target.
			{From: handleGet, To: "boundary:db INSERT users", Boundary: "db"},
		},
	}
	ix := graph.NewIndex(g)
	// io_budget with NO layering: RootPackages() is empty.
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 1}}

	if IsRoute(p, ix, serverMain) {
		t.Errorf("main is a composition root, not a route — IsRoute must be false")
	}
	if !IsRoute(p, ix, handleGet) {
		t.Errorf("HandleGet is a caller-less non-root entrypoint — IsRoute must be true")
	}

	routes := RouteWrites(p, ix)
	if _, charged := routes[serverMain]; charged {
		t.Errorf("main charged as a route; its startup migrations would inflate the budget: %v", routes)
	}
	if _, ok := routes[handleGet]; !ok {
		t.Fatalf("real route HandleGet missing from RouteWrites: %v", routes)
	}

	// The enforcer must not fire io_budget on the route (1 write, budget 1) and
	// must not have charged main's two startup writes to anyone.
	res := Check(p, ix)
	for _, v := range res.Violations() {
		if v.Rule == "io_budget" {
			t.Errorf("io_budget violated though the only route writes exactly one target: %s", v.Summary)
		}
	}
}

// The enforcer iterates ix.Sources(); IsRoute over those same sources must select
// EXACTLY the RouteWrites key set — the card cannot claim a rule binds a function
// the enforcer never charges (H-8).
func TestIsRouteMatchesRouteWritesScope(t *testing.T) {
	const (
		serverMain = "example.com/svc/cmd/server.main"
		handleGet  = "example.com/svc/internal/handler.HandleGet"
	)
	g := &graph.Graph{
		CompositionRoots: []string{"example.com/svc/cmd/server"},
		Nodes: []graph.Node{
			{FQN: serverMain, Sig: "func()", Tier: 3},
			{FQN: handleGet, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: handleGet, To: "boundary:db INSERT users", Boundary: "db"},
		},
	}
	ix := graph.NewIndex(g)
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 5}}

	routes := RouteWrites(p, ix)
	for _, src := range ix.Sources() {
		_, charged := routes[src]
		if want := IsRoute(p, ix, src); charged != want {
			t.Errorf("scope disagreement on %s: RouteWrites charged=%v, IsRoute=%v", src, charged, want)
		}
	}
}
