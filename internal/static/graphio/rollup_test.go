package graphio

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// rollupSampleGraph is a hand-built graph exercising every rollup edge class: a
// cross-package call, an intra-package call (must NOT become a component edge), two
// resolved boundary effects, an effect-bearing ExternalBoundaryCall (a disclosed dashed
// edge, with a human annotation), a TRIVIAL EBC (must be excluded as plumbing), and a
// synthetic node with no package (must anchor nothing).
func rollupSampleGraph() *Graph {
	const (
		serve = "(*ex.com/svc/handler.H).Serve"
		save  = "(*ex.com/svc/store.S).Save"
		newS  = "ex.com/svc/store.New"
	)
	return &Graph{
		Nodes: []Node{
			{FQN: serve, Package: "ex.com/svc/handler"},
			{FQN: save, Package: "ex.com/svc/store"},
			{FQN: newS, Package: "ex.com/svc/store"},
			{FQN: "ex.com/svc/store.New$1", Package: ""}, // synthetic — not a component
		},
		Edges: []Edge{
			{From: serve, To: save},                       // cross-package call → component edge
			{From: save, To: newS},                        // intra-package → NOT a component edge
			{From: save, To: "boundary:db INSERT ledger"}, // resolved effect store→db
			{From: serve, To: "boundary:bus PUBLISH x"},   // resolved effect handler→bus
		},
		BlindSpots: []blindspots.BlindSpot{
			{Kind: blindspots.ExternalBoundaryCall, Site: serve, Package: "github.com/customerio/go-customerio", Severity: blindspots.SeverityEffectBearing},
			{Kind: blindspots.ExternalBoundaryCall, Site: save, Package: "github.com/google/uuid", Severity: blindspots.SeverityTrivial}, // plumbing — excluded
		},
		Annotations: []Annotation{
			{Site: serve, Kind: "ExternalBoundaryCall", Claim: "POSTs to track.customer.io"},
		},
	}
}

func TestRollupByPackage(t *testing.T) {
	r := rollupSampleGraph().RollupByPackage()

	wantComponents := []Component{
		{Package: "ex.com/svc/handler", Name: "handler", Nodes: 1, Band: BandTransport},
		{Package: "ex.com/svc/store", Name: "store", Nodes: 2, Band: BandStorage}, // Save + New; the synthetic $1 is excluded
	}
	if !reflect.DeepEqual(r.Components, wantComponents) {
		t.Errorf("components =\n%+v\nwant\n%+v", r.Components, wantComponents)
	}

	wantEdges := []RollupEdge{
		{From: "ex.com/svc/handler", To: "bus", Kind: RollupEffect},
		{From: "ex.com/svc/handler", To: "ex.com/svc/store", Kind: RollupCall},
		{From: "ex.com/svc/handler", To: "github.com/customerio/go-customerio", Kind: RollupDisclosed, Note: "POSTs to track.customer.io"},
		{From: "ex.com/svc/store", To: "db", Kind: RollupEffect},
	}
	if !reflect.DeepEqual(r.Edges, wantEdges) {
		t.Errorf("edges =\n%+v\nwant\n%+v", r.Edges, wantEdges)
	}
}

// compositionRootGraph models the DI-back-edge pattern the wiring class exists for: a
// `cmd/<svc>` composition root (package main) that CONSTRUCTS a server (cmd→server, a
// forward call) and INJECTS a closure into it that the server later invokes
// (server→cmd via the closure literal `cmd/svc.newHandler$1`, a back-edge). The
// composition root is recognized by its package-level `main` node (FQN == pkg+".main").
func compositionRootGraph() *Graph {
	const (
		mainFn  = "ex.com/svc/cmd/svc.main"
		closure = "ex.com/svc/cmd/svc.newHandler$1"
		newSrv  = "ex.com/svc/server.New"
		serve   = "(*ex.com/svc/server.S).Serve"
	)
	return &Graph{
		Nodes: []Node{
			{FQN: mainFn, Package: "ex.com/svc/cmd/svc"},
			{FQN: closure, Package: "ex.com/svc/cmd/svc"}, // a closure DEFINED in main
			{FQN: newSrv, Package: "ex.com/svc/server"},
			{FQN: serve, Package: "ex.com/svc/server"},
		},
		Edges: []Edge{
			{From: mainFn, To: newSrv}, // cmd → server: constructor wiring (stays a call)
			{From: serve, To: closure}, // server → cmd: injected-closure BACK-EDGE → wiring
		},
	}
}

// TestRollupMarksCompositionRoot pins both halves of the composition-root ask: the
// `package main` component is flagged role="composition_root", and the X→cmd
// injected-closure back-edge is classified "wiring" (not "call"), while the cmd→X
// constructor edge stays a plain call. The recognition convention (FQN == pkg+".main")
// is the parity partner of groundwork/fitness.proposeLayers.
func TestRollupMarksCompositionRoot(t *testing.T) {
	r := compositionRootGraph().RollupByPackage()

	wantComponents := []Component{
		{Package: "ex.com/svc/cmd/svc", Name: "svc", Nodes: 2, Role: RollupRoot}, // root: named by Role, left bandless
		{Package: "ex.com/svc/server", Name: "server", Nodes: 2, Band: BandTransport},
	}
	if !reflect.DeepEqual(r.Components, wantComponents) {
		t.Errorf("components =\n%+v\nwant\n%+v", r.Components, wantComponents)
	}

	wantEdges := []RollupEdge{
		{From: "ex.com/svc/cmd/svc", To: "ex.com/svc/server", Kind: RollupCall},   // constructor wiring stays a call
		{From: "ex.com/svc/server", To: "ex.com/svc/cmd/svc", Kind: RollupWiring}, // injected-closure back-edge
	}
	if !reflect.DeepEqual(r.Edges, wantEdges) {
		t.Errorf("edges =\n%+v\nwant\n%+v", r.Edges, wantEdges)
	}

	// A wiring edge targets a first-party component, so it is NOT external (it must
	// resolve to a component box, never a stadium node).
	for _, e := range r.Edges {
		if e.Kind == RollupWiring && e.External() {
			t.Errorf("a wiring edge must not be External(): %+v", e)
		}
	}
}

// TestRollupCompositionRootsFieldIsAuthoritative pins that when the graph carries the
// authoritative CompositionRoots field (set at build from roots.KindMain), the rollup
// trusts it and does NOT fall back to the FQN heuristic — so a non-main package that
// merely declares a package-level `func main` is correctly NOT a composition root, and
// a real call into it stays a domain `call` rather than being misclassified as wiring.
// This is the soundness fix: the FQN heuristic alone would mark `util` a root here.
func TestRollupCompositionRootsFieldIsAuthoritative(t *testing.T) {
	g := &Graph{
		CompositionRoots: []string{"ex.com/svc/cmd/svc"}, // authoritative: only this is main
		Nodes: []Node{
			{FQN: "ex.com/svc/cmd/svc.main", Package: "ex.com/svc/cmd/svc"},
			{FQN: "ex.com/svc/util.main", Package: "ex.com/svc/util"}, // a non-main pkg with func main (a smell, but importable)
			{FQN: "ex.com/svc/app.Run", Package: "ex.com/svc/app"},
		},
		Edges: []Edge{
			{From: "ex.com/svc/app.Run", To: "ex.com/svc/util.main"}, // a REAL call into util — must stay a call
		},
	}
	r := g.RollupByPackage()
	for _, c := range r.Components {
		switch c.Package {
		case "ex.com/svc/cmd/svc":
			if c.Role != RollupRoot {
				t.Errorf("the listed main package must be the composition root, got Role=%q", c.Role)
			}
		case "ex.com/svc/util":
			if c.Role != "" {
				t.Errorf("a non-main package with a func main must NOT be a root when CompositionRoots is authoritative: %+v", c)
			}
		}
	}
	for _, e := range r.Edges {
		if e.From == "ex.com/svc/app" && e.To == "ex.com/svc/util" && e.Kind != RollupCall {
			t.Errorf("a real call into util must stay a domain call, not wiring: %+v", e)
		}
	}
}

// TestRollupMethodNamedMainIsNotARoot pins the precision of the legacy FALLBACK
// recognition (a graph with no CompositionRoots field, e.g. a base built before the
// field existed): a METHOD named main ("(*pkg.T).main") is not a package `func main`,
// so its package is not a composition root and an edge into it stays a plain call. The
// fallback is intentionally stricter than groundwork/fitness.proposeLayers, which would
// over-match this case.
func TestRollupMethodNamedMainIsNotARoot(t *testing.T) {
	g := &Graph{
		Nodes: []Node{
			{FQN: "(*ex.com/svc/widget.T).main", Package: "ex.com/svc/widget"},
			{FQN: "ex.com/svc/caller.Run", Package: "ex.com/svc/caller"},
		},
		Edges: []Edge{{From: "ex.com/svc/caller.Run", To: "(*ex.com/svc/widget.T).main"}},
	}
	r := g.RollupByPackage()
	for _, c := range r.Components {
		if c.Role != "" {
			t.Errorf("a method named main must not mark its package a composition root: %+v", c)
		}
	}
	for _, e := range r.Edges {
		if e.Kind != RollupCall {
			t.Errorf("edge into a (method-named-main) package must be a plain call, got %+v", e)
		}
	}
}

// TestRollupDiffWiringNotCode pins the diff-pollution fix: swapping WHICH closure the
// composition root injects (server→cmd back-edge target changes) lands in Wiring*, NOT
// in Code*, so a pure re-wiring never reads as a domain dependency added/removed.
func TestRollupDiffWiringNotCode(t *testing.T) {
	base := compositionRootGraph().RollupByPackage()

	// Branch: main injects a DIFFERENT closure ($2 instead of $1) — same back-edge at
	// the COMPONENT altitude (server→cmd), so the collapsed edge is identical and there
	// is no delta at all. To force a wiring DELTA, drop the back-edge entirely (the
	// server no longer invokes any injected closure).
	branchGraph := compositionRootGraph()
	branchGraph.Edges = branchGraph.Edges[:1] // keep cmd→server, drop server→cmd wiring
	branch := branchGraph.RollupByPackage()

	d := diffRollups(base, branch)
	if len(d.WiringRemoved) != 1 || d.WiringRemoved[0].Kind != RollupWiring {
		t.Errorf("dropping the injected-closure back-edge must be ONE wiring removal, got %+v", d.WiringRemoved)
	}
	if len(d.CodeAdded)+len(d.CodeRemoved) != 0 {
		t.Errorf("a wiring change must not pollute the code bin, got added=%+v removed=%+v", d.CodeAdded, d.CodeRemoved)
	}

	// Symmetry: base↔branch swap flips WiringRemoved into WiringAdded.
	rev := diffRollups(branch, base)
	if !reflect.DeepEqual(rev.WiringAdded, d.WiringRemoved) || !reflect.DeepEqual(rev.WiringRemoved, d.WiringAdded) {
		t.Errorf("wiring diff is not symmetric under swap:\nfwd=%+v\nrev=%+v", d, rev)
	}
}

// TestRollupCompositionRootMermaidValid pins that the composition-root rollup render —
// which introduces the :::root component class and the dotted "wires" back-edge — is
// structurally valid Mermaid (every referenced class is defined, no leaked labels) in
// both the plain and diff views.
func TestRollupCompositionRootMermaidValid(t *testing.T) {
	g := compositionRootGraph()
	if err := validateMermaid(g.RollupByPackage().Mermaid()); err != nil {
		t.Errorf("composition-root rollup Mermaid invalid: %v", err)
	}
	branch := compositionRootGraph()
	branch.Edges = branch.Edges[:1]
	if err := validateMermaid(RollupMermaidDiff(g, branch)); err != nil {
		t.Errorf("composition-root rollup diff Mermaid invalid: %v", err)
	}
}

// TestRollupExcludesTrivialEBC pins that a trivial (plumbing-tier) ExternalBoundaryCall
// is NOT a disclosed component edge — the component view's signal depends on the same
// Severity split the func()-seam tiering uses.
func TestRollupExcludesTrivialEBC(t *testing.T) {
	for _, e := range rollupSampleGraph().RollupByPackage().Edges {
		if strings.Contains(e.To, "uuid") {
			t.Errorf("a trivial EBC (uuid) must not appear as a disclosed component edge: %+v", e)
		}
	}
}

// TestRollupDeterministic is the determinism guard the rollup ordering ships with
// (CLAUDE.md: a new canonicalization path ships with a determinism test). The grouping
// walks maps (package counts, the edge-dedup set), so any arrival-order leak would
// surface as a run-to-run difference in either the JSON model or the Mermaid render.
func TestRollupDeterministic(t *testing.T) {
	g := rollupSampleGraph()
	base := rollupSampleGraph() // a second graph for the diff render path
	first := g.RollupByPackage()
	firstMermaid := first.Mermaid()
	firstDiffMermaid := RollupMermaidDiff(base, g)
	for i := 0; i < 50; i++ {
		got := g.RollupByPackage()
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("rollup model not deterministic on run %d:\n%+v\nvs\n%+v", i, got, first)
		}
		if m := got.Mermaid(); m != firstMermaid {
			t.Fatalf("rollup Mermaid not deterministic on run %d:\n%s\nvs\n%s", i, m, firstMermaid)
		}
		// The diff render is its own ordering path (union node-id allocation + linkStyle
		// index assignment), so it ships with its own determinism check.
		if m := RollupMermaidDiff(base, g); m != firstDiffMermaid {
			t.Fatalf("rollup diff Mermaid not deterministic on run %d:\n%s\nvs\n%s", i, m, firstDiffMermaid)
		}
	}
}

// TestRollupDiffSplitAndSymmetry pins the code-vs-disclosure split and the symmetry
// invariant: swapping base and branch must flip every Added into the matching Removed.
// The split is what keeps the diff honest — a newly-documented blind effect (disclosure)
// must never be counted as a new real dependency (code).
func TestRollupDiffSplitAndSymmetry(t *testing.T) {
	base := rollupSampleGraph().RollupByPackage()

	// Branch: drop the handler→store call (a real dependency removed), keep everything
	// else, and add a NEW disclosed effect (a newly-documented blind handoff).
	branchGraph := rollupSampleGraph()
	branchGraph.Edges = branchGraph.Edges[1:] // drop the serve→save cross-package call
	branchGraph.BlindSpots = append(branchGraph.BlindSpots, blindspots.BlindSpot{
		Kind: blindspots.ExternalBoundaryCall, Site: "(*ex.com/svc/store.S).Save",
		Package: "github.com/stripe/stripe-go", Severity: blindspots.SeverityEffectBearing,
	})
	branch := branchGraph.RollupByPackage()

	d := diffRollups(base, branch)
	if len(d.CodeRemoved) != 1 || d.CodeRemoved[0].Kind != RollupCall {
		t.Errorf("dropping the cross-package call must be ONE code removal, got %+v", d.CodeRemoved)
	}
	if len(d.DisclosureAdded) != 1 || d.DisclosureAdded[0].To != "github.com/stripe/stripe-go" {
		t.Errorf("the new blind handoff must be ONE disclosure addition, got %+v", d.DisclosureAdded)
	}
	if len(d.CodeAdded) != 0 {
		t.Errorf("no real dependency was added; code_added must be empty, got %+v", d.CodeAdded)
	}

	// Symmetry: base↔branch swap flips Added↔Removed exactly.
	rev := diffRollups(branch, base)
	if !reflect.DeepEqual(rev.CodeAdded, d.CodeRemoved) || !reflect.DeepEqual(rev.CodeRemoved, d.CodeAdded) ||
		!reflect.DeepEqual(rev.DisclosureAdded, d.DisclosureRemoved) || !reflect.DeepEqual(rev.DisclosureRemoved, d.DisclosureAdded) {
		t.Errorf("diff is not symmetric under base↔branch swap:\nfwd=%+v\nrev=%+v", d, rev)
	}
}

// TestRollupDiffNoteOnlyChangeIsNotADelta pins that a disclosed edge present in BOTH
// sides that differs only in its annotation note is NOT a delta — edge identity is
// (From, To, Kind), so the effect did not change; only its human context did. This is the
// boundary the code-vs-disclosure split rests on: a re-worded note is not a new effect.
func TestRollupDiffNoteOnlyChangeIsNotADelta(t *testing.T) {
	base := rollupSampleGraph().RollupByPackage()

	branchGraph := rollupSampleGraph()
	branchGraph.Annotations = []Annotation{
		{Site: "(*ex.com/svc/handler.H).Serve", Kind: "ExternalBoundaryCall", Claim: "POSTs to a DIFFERENT host"},
	}
	branch := branchGraph.RollupByPackage()

	// The disclosed edge's Note differs between the two rollups...
	if base.Edges[2].Note == branch.Edges[2].Note {
		t.Fatalf("test setup: expected the disclosed edge note to differ, both = %q", base.Edges[2].Note)
	}
	// ...yet the diff reports nothing, in either direction.
	d := diffRollups(base, branch)
	if len(d.CodeAdded)+len(d.CodeRemoved)+len(d.DisclosureAdded)+len(d.DisclosureRemoved) != 0 {
		t.Errorf("a note-only change must produce no delta, got %+v", d)
	}
}

// TestRollupDiffDisclosesSkew pins the honesty channel the rollup diff shares with the
// call-graph diff: a base↔branch SUBSTRATE skew (empty base, --algo mismatch, or a
// package-less/old base) is disclosed as a caveat rather than silently painted as a
// code/disclosure delta — the confidently-wrong delta the prime directive forbids. A
// clean diff stays caveat-free.
func TestRollupDiffDisclosesSkew(t *testing.T) {
	hasCaveat := func(cs []string, want string) bool {
		for _, c := range cs {
			if strings.Contains(c, want) {
				return true
			}
		}
		return false
	}
	branch := rollupSampleGraph()
	branch.Algo = "vta"

	if d := RollupDiff(&Graph{Algo: "vta"}, branch); !hasCaveat(d.Caveats, "empty") {
		t.Errorf("an empty base must be disclosed, got %v", d.Caveats)
	}
	rtaBase := rollupSampleGraph()
	rtaBase.Algo = "rta"
	if d := RollupDiff(rtaBase, branch); !hasCaveat(d.Caveats, "algo differs") {
		t.Errorf("an algo skew must be disclosed, got %v", d.Caveats)
	}
	pkgless := &Graph{Algo: "vta", Nodes: []Node{{FQN: "x.F"}}}
	if d := RollupDiff(pkgless, branch); !hasCaveat(d.Caveats, "no package facts") {
		t.Errorf("a package-less base must be disclosed, got %v", d.Caveats)
	}
	if d := RollupDiff(branch, branch); len(d.Caveats) != 0 {
		t.Errorf("a clean same-graph diff must be caveat-free, got %v", d.Caveats)
	}
}
