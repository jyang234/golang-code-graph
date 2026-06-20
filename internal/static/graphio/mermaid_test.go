package graphio

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// sampleGraph mirrors the shape of a real loansvc graph closely enough to exercise
// every render branch: a fallible node, a tier-3 plumbing node, the three boundary
// effect kinds (db/bus/external), an async edge, a reclaimed edge, a blind spot, and
// a frontier marker.
func sampleGraph() *Graph {
	return &Graph{
		Entrypoint: "POST /loan-application",
		Algo:       "rta",
		Nodes: []Node{
			{FQN: "(*example.com/loansvc/internal/handler.App).Create", Sig: "func()", Tier: 1},
			{FQN: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", Sig: "func() error", Tier: 2, Fallible: true},
			{FQN: "example.com/loansvc/internal/origination.tracer", Sig: "func()", Tier: 3},
		},
		Edges: []Edge{
			{From: "(*example.com/loansvc/internal/handler.App).Create", To: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", Tier: 2},
			{From: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", To: "boundary:bus PUBLISH loan.approved", Tier: 1, Boundary: "outbound-async"},
			{From: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", To: "boundary:db INSERT ledger", Tier: 1, Boundary: "outbound-sync"},
			{From: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", To: "boundary:payment-gw POST /charge/{id}", Tier: 1, Boundary: "outbound-sync", Via: "strict-server"},
			{From: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", To: "example.com/loansvc/internal/origination.tracer", Tier: 3},
		},
		BlindSpots: []blindspots.BlindSpot{
			{Kind: blindspots.Reflect, Site: "(*example.com/loansvc/internal/origination.Evaluator).Evaluate", Detail: "reflective call"},
		},
		Frontier: &FrontierSection{
			Markers: []frontier.Marker{
				{Kind: "dynamic-bus", Bin: frontier.BinA, Site: "bus PUBLISH <dynamic>", Owner: "(*example.com/loansvc/internal/handler.App).Create"},
			},
		},
	}
}

func TestMermaidDeterministic(t *testing.T) {
	g := sampleGraph()
	first := g.Mermaid(MermaidOptions{MaxTier: 2})
	for i := 0; i < 8; i++ {
		if got := g.Mermaid(MermaidOptions{MaxTier: 2}); got != first {
			t.Fatalf("Mermaid not deterministic on run %d", i)
		}
	}
	if !strings.HasPrefix(first, "flowchart LR\n") {
		t.Fatalf("want flowchart LR header, got:\n%s", first)
	}
	if !strings.HasSuffix(first, "\n") {
		t.Fatalf("output must end with a newline")
	}
}

func TestMermaidStructure(t *testing.T) {
	out := sampleGraph().Mermaid(MermaidOptions{MaxTier: 2})

	wantContains := []string{
		"%% static call graph — scope: POST /loan-application; algo: rta",
		`["origination.Evaluator.Evaluate ⚠"]:::fallible`, // fallible node marked
		`{{"bus PUBLISH loan.approved"}}:::bus`,           // bus → hexagon
		`[("db INSERT ledger")]:::db`,                     // db → cylinder
		`(["payment-gw POST /charge/{id}"]):::external`,   // external → stadium
		"-. async .->",      // outbound-async drawn dashed
		"via strict-server", // reclaimed-edge provenance shown
		"⊥ reflect",         // blind spot rendered as a terminal node
		"⌖ dynamic-bus",     // frontier marker rendered
		"-. blind .->",      // disclosure attached to its site/owner
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n--- full output ---\n%s", w, out)
		}
	}
}

// TestMermaidEscapesAngleBrackets pins the fix for HTML-label corruption: a data
// label containing '<'/'>' (the "<dynamic>" effect marker) must be escaped, or
// Mermaid's HTML-label mode parses it as a dropped tag and blanks the label — the
// dynamic-publish disclosure rendering as nothing.
func TestMermaidEscapesAngleBrackets(t *testing.T) {
	g := &Graph{
		Algo:  "rta",
		Nodes: []Node{{FQN: "example.com/s/n.Emit", Tier: 1}},
		Edges: []Edge{{From: "example.com/s/n.Emit", To: "boundary:bus PUBLISH <dynamic>", Tier: 1, Boundary: "outbound-async"}},
	}
	out := g.Mermaid(MermaidOptions{MaxTier: 2})
	if strings.Contains(out, "<dynamic>") {
		t.Errorf("raw <dynamic> would be eaten by Mermaid's HTML labels; must be escaped:\n%s", out)
	}
	if !strings.Contains(out, "bus PUBLISH &lt;dynamic&gt;") {
		t.Errorf("expected escaped label bus PUBLISH &lt;dynamic&gt;:\n%s", out)
	}
}

func TestMermaidCollapsesPlumbingByDefault(t *testing.T) {
	g := sampleGraph()

	collapsed := g.Mermaid(MermaidOptions{MaxTier: 2})
	if strings.Contains(collapsed, "origination.tracer") {
		t.Errorf("tier-3 tracer should be collapsed at MaxTier=2:\n%s", collapsed)
	}
	if !strings.Contains(collapsed, "hidden as plumbing") {
		t.Errorf("collapse must be disclosed in a header comment:\n%s", collapsed)
	}

	full := g.Mermaid(MermaidOptions{MaxTier: 0})
	if !strings.Contains(full, "origination.tracer") {
		t.Errorf("MaxTier=0 should show the tracer:\n%s", full)
	}
	if strings.Contains(full, "hidden as plumbing") {
		t.Errorf("nothing hidden at MaxTier=0, so no disclosure expected:\n%s", full)
	}
}

// TestMermaidAllBlindSpotsRestoresOrphans pins the --all-blind-spots escape hatch: the
// denoised default rolls plumbing-tier disclosures (trivial boundaries and those
// orphaned onto collapsed plumbing) into a counted header note, but ShowAllBlindSpots
// must draw every disclosure node back in WITHOUT un-collapsing the plumbing nodes.
func TestMermaidAllBlindSpotsRestoresOrphans(t *testing.T) {
	// An orphaned disclosure: a reflect blind spot at a package-level site with no
	// first-party node to attach to, so the default denoise drops + counts it.
	g := &Graph{
		Algo:       "rta",
		Nodes:      []Node{{FQN: "example.com/svc/internal/handler.H", Tier: 1}},
		BlindSpots: []blindspots.BlindSpot{{Kind: blindspots.Reflect, Site: "example.com/svc/internal/rawmem"}},
	}

	denoised := g.Mermaid(MermaidOptions{MaxTier: 2})
	if strings.Contains(denoised, "⊥ reflect") {
		t.Errorf("default denoise should drop the orphaned reflect disclosure into a count:\n%s", denoised)
	}
	if !strings.Contains(denoised, "on hidden plumbing omitted; pass --all-blind-spots to include") {
		t.Errorf("the dropped disclosure must be disclosed in a counted note naming the escape hatch:\n%s", denoised)
	}

	full := g.Mermaid(MermaidOptions{MaxTier: 2, ShowAllBlindSpots: true})
	if !strings.Contains(full, "⊥ reflect") {
		t.Errorf("--all-blind-spots must draw the orphaned reflect disclosure back in:\n%s", full)
	}
	if strings.Contains(full, "on hidden plumbing omitted") {
		t.Errorf("with every disclosure shown there is nothing to omit, so no count note:\n%s", full)
	}
}

// TestMermaidTiersTrivialFuncSeam pins the func() disclosure-channel hygiene in the
// rendered view: a context.CancelFunc UnresolvedCall is tagged trivial, so the default
// tier-collapsed render drops it as plumbing (counted, recoverable) exactly like a
// trivial boundary — and --all-blind-spots draws it back WITH its tier named, so a
// reader sees why it was collapsible instead of a bare "blind spot" box. Before func()
// seams could be trivial, no UnresolvedCall ever reached this path.
func TestMermaidTiersTrivialFuncSeam(t *testing.T) {
	site := "example.com/svc/internal/api.handle"
	g := &Graph{
		Algo:  "rta",
		Nodes: []Node{{FQN: site, Tier: 1}},
		BlindSpots: []blindspots.BlindSpot{{
			Kind:     blindspots.UnresolvedCall,
			Site:     site, // a drawn node → dropped as trivial plumbing, not as an orphan
			Detail:   "a func-value call of type context.CancelFunc resolved to no callee; the invoked function and its downstream edges are invisible to the static call graph",
			Severity: blindspots.SeverityTrivial,
		}},
	}

	denoised := g.Mermaid(MermaidOptions{MaxTier: 2})
	if strings.Contains(denoised, "⊥ UnresolvedCall") {
		t.Errorf("a trivial context.CancelFunc seam should collapse as plumbing in the default render:\n%s", denoised)
	}

	full := g.Mermaid(MermaidOptions{MaxTier: 2, ShowAllBlindSpots: true})
	if !strings.Contains(full, "⊥ UnresolvedCall") {
		t.Errorf("--all-blind-spots must draw the trivial func() seam back in:\n%s", full)
	}
	if !strings.Contains(full, "trivial") {
		t.Errorf("a shown func() seam must name its plumbing tier, not a bare label:\n%s", full)
	}
}

// TestMermaidNeverHidesEffectEmitter pins the soundness rule: a node that emits a
// boundary effect is shown even when its tier would otherwise collapse it, so the
// denoised view can never silently drop an effect.
func TestMermaidNeverHidesEffectEmitter(t *testing.T) {
	g := &Graph{
		Algo:  "rta",
		Nodes: []Node{{FQN: "example.com/svc/internal/log.flush", Tier: 3}},
		Edges: []Edge{{From: "example.com/svc/internal/log.flush", To: "boundary:db UPDATE audit", Tier: 1, Boundary: "outbound-sync"}},
	}
	out := g.Mermaid(MermaidOptions{MaxTier: 2})
	if !strings.Contains(out, "log.flush") {
		t.Errorf("a tier-3 node that emits an effect must NOT be hidden:\n%s", out)
	}
	if !strings.Contains(out, `[("db UPDATE audit")]`) {
		t.Errorf("the effect node must be present:\n%s", out)
	}
}
