package fitness

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// A graph spanning two binaries has two composition roots. proposeLayers must
// exempt and record BOTH — keying on the last .main iterated (ix.Nodes() is
// sorted) silently dropped all but the alphabetically-final root.
func TestProposeLayersMultipleRoots(t *testing.T) {
	const (
		serverMain = "example.com/svc/cmd/server.main"
		workerMain = "example.com/svc/cmd/worker.main"
		appRun     = "example.com/svc/internal/app.Run"
		storeSave  = "example.com/svc/internal/store.Save"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: serverMain, Sig: "func()", Tier: 3},
			{FQN: workerMain, Sig: "func()", Tier: 3},
			{FQN: appRun, Sig: "func()", Tier: 3},
			{FQN: storeSave, Sig: "func() error", Tier: 3},
		},
		Edges: []graph.Edge{
			{From: serverMain, To: appRun, Tier: 3},
			{From: workerMain, To: appRun, Tier: 3},
			{From: appRun, To: storeSave, Tier: 3},
		},
	}
	p, _ := Propose(graph.NewIndex(g), "svc")
	if p.Layering == nil {
		t.Fatal("expected a layering proposal")
	}
	got := map[string]bool{}
	for _, r := range p.Layering.Roots {
		got[r] = true
	}
	for _, want := range []string{"example.com/svc/cmd/server", "example.com/svc/cmd/worker"} {
		if !got[want] {
			t.Errorf("root %q missing from proposed roots %v", want, p.Layering.Roots)
		}
	}
}

// TestProposeLayersPrefersAuthoritativeRoots pins that proposeLayers trusts the
// graph's authoritative CompositionRoots (flowmap's roots.KindMain) over its
// structural `.main` heuristic: a non-main package that declares a package-level
// `func main` (a smell, but legal and importable) is NOT exempted as a composition
// root, so a real dependency layer is not silently dropped from layer ranking. The
// FQN heuristic alone would over-match `util` here.
func TestProposeLayersPrefersAuthoritativeRoots(t *testing.T) {
	const (
		serverMain = "example.com/svc/cmd/server.main"
		utilMain   = "example.com/svc/internal/util.main" // non-main pkg with a func main
		appRun     = "example.com/svc/internal/app.Run"
		storeSave  = "example.com/svc/internal/store.Save"
	)
	g := &graph.Graph{
		CompositionRoots: []string{"example.com/svc/cmd/server"}, // authoritative: only this is main
		Nodes: []graph.Node{
			{FQN: serverMain, Sig: "func()", Tier: 3},
			{FQN: utilMain, Sig: "func()", Tier: 3},
			{FQN: appRun, Sig: "func()", Tier: 3},
			{FQN: storeSave, Sig: "func() error", Tier: 3},
		},
		Edges: []graph.Edge{
			{From: serverMain, To: appRun, Tier: 3},
			{From: appRun, To: storeSave, Tier: 3},
			{From: appRun, To: utilMain, Tier: 3}, // app depends on util — a real layer, not a root
		},
	}
	p, _ := Propose(graph.NewIndex(g), "svc")
	if p.Layering == nil {
		t.Fatal("expected a layering proposal")
	}
	got := map[string]bool{}
	for _, r := range p.Layering.Roots {
		got[r] = true
	}
	if !got["example.com/svc/cmd/server"] {
		t.Errorf("authoritative composition root missing from proposed roots %v", p.Layering.Roots)
	}
	if got["example.com/svc/internal/util"] {
		t.Errorf("util declares a func main but is NOT a composition root per the authoritative field; it must not be exempted as a root: %v", p.Layering.Roots)
	}
}
