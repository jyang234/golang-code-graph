package review

import "testing"

const (
	pkgHandler = "example.com/layeredsvc/internal/handler"
	pkgApp     = "example.com/layeredsvc/internal/app"
)

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
