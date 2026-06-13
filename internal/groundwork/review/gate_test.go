package review

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

const (
	pkgHandler = "example.com/layeredsvc/internal/handler"
	pkgApp     = "example.com/layeredsvc/internal/app"
)

// dbCallPair builds a base/branch graph (identical unless an edge is added) whose
// one route mutates through a "db call" wrapper — so the io_budget is
// unenforceable on BOTH sides. The caution is therefore standing, never new: it
// is exactly the steady-state case the base→branch delta suppresses (R1).
func dbCallPair() (*policy.Policy, *graph.Graph) {
	const (
		route = "(*example.com/svc/internal/handler.Server).Do"
		store = "(*example.com/svc/internal/store.Store).Save"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: store, Tier: 2},
			{From: store, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 2}}
	return p, g
}

// The crux of R1: a passing gate over a graph with a steady-state unenforceable
// budget must SURFACE the standing caution — pre-fix it was silent because the
// caution holds identically on base and branch and the delta drops it.
func TestGateSurfacesStandingCautionOnPass(t *testing.T) {
	p, g := dbCallPair()
	res := Gate(p, g, g, nil) // base == branch: no delta, a passing gate

	if !res.Pass {
		t.Fatalf("a standing caution must not block the gate; got %v", res.NewViolations)
	}
	if len(res.StandingCautions) == 0 {
		t.Fatal("the gate must disclose the standing io_budget caution, not pass silently")
	}
	var found bool
	for _, c := range res.StandingCautions {
		if c.Rule == "io_budget" && strings.Contains(c.Summary, "unenforceable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a standing io_budget unenforceable caution, got %v", res.StandingCautions)
	}
	if !strings.Contains(res.Render(), "standing caution") {
		t.Errorf("the human gate report must show the standing caution; got:\n%s", res.Render())
	}
}

func TestGateNewViolationBlocks(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	g := Gate(p, base, branch, nil)
	if g.Pass {
		t.Fatal("a new layering violation must block the gate")
	}
	if len(g.NewViolations) != 1 || g.NewViolations[0].Rule != "layering" {
		t.Fatalf("want the new layering violation, got %v", g.NewViolations)
	}
}

func TestGateCleanPasses(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-good.graph.json")
	if g := Gate(p, base, branch, nil); !g.Pass {
		t.Fatalf("the correctly-wired branch must pass; got violations=%v escapes=%v breaking=%v",
			g.NewViolations, g.ScopeEscapes, g.BreakingContract)
	}
}

func TestGateScopeEscapeBlocks(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-good.graph.json")
	// The new endpoint wires handler→app, so it touches both packages; a scope of
	// handler-only is an escape on app.
	g := Gate(p, base, branch, []string{pkgHandler})
	if g.Pass {
		t.Fatal("a touched package outside the declared scope must block")
	}
	if len(g.ScopeEscapes) != 1 || g.ScopeEscapes[0] != pkgApp {
		t.Fatalf("scope escapes = %v, want [%s]", g.ScopeEscapes, pkgApp)
	}
}

func TestGateScopeWithinPasses(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-good.graph.json")
	if g := Gate(p, base, branch, []string{pkgHandler, pkgApp}); !g.Pass {
		t.Fatalf("a change confined to the declared scope must pass; got %v", g.ScopeEscapes)
	}
}

func TestGateDeterministicDigest(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	if a, b := Gate(p, base, branch, nil), Gate(p, base, branch, nil); a.Digest != b.Digest {
		t.Fatalf("non-deterministic gate digest: %s vs %s", a.Digest, b.Digest)
	}
}
