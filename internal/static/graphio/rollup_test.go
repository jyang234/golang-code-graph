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
		{Package: "ex.com/svc/handler", Name: "handler", Nodes: 1},
		{Package: "ex.com/svc/store", Name: "store", Nodes: 2}, // Save + New; the synthetic $1 is excluded
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
