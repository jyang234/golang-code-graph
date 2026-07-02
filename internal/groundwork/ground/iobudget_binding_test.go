package ground

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// H-8: the ground card must bind io_budget through the enforcer's own route
// predicate (fitness.IsRoute), not a hand-rolled "caller-less function" test. A
// composition-root main is caller-less but is NOT a route — checkIOBudget will
// never charge it — so the card must not tell an agent editing main that the
// budget guardrail binds it. A real caller-less route must still be bound.
func TestIOBudgetBindingUsesRoutePredicate(t *testing.T) {
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
	// io_budget with no layering — RootPackages() is empty, so only the graph's
	// authoritative CompositionRoots distinguishes main from a route.
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 3}}

	mainCard, err := For(ix, p, serverMain)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range mainCard.Binding {
		if strings.Contains(b, "io_budget") {
			t.Errorf("card claims io_budget binds the composition root main, but the enforcer never charges it: %v", mainCard.Binding)
		}
	}

	routeCard, err := For(ix, p, handleGet)
	if err != nil {
		t.Fatal(err)
	}
	var bound bool
	for _, b := range routeCard.Binding {
		if strings.Contains(b, "io_budget") {
			bound = true
		}
	}
	if !bound {
		t.Errorf("card omits io_budget on a real route the enforcer charges: %v", routeCard.Binding)
	}
}
