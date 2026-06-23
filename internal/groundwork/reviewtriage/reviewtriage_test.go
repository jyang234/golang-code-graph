package reviewtriage

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// TestNewBlindVsAccounted pins the core split: a changed function with a fully-resolved
// effect surface is ACCOUNTED (complete evidence rendered), while a changed function that
// introduces a blind spot (the base had none) is NEW BLIND (reason + FLOOR rendered).
func TestNewBlindVsAccounted(t *testing.T) {
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "old"}, {FQN: "svc.Blind", Sig: "old"}},
	}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "new"}, {FQN: "svc.Blind", Sig: "new"}},
		Edges: []graph.Edge{
			{From: "svc.Clean", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{
			{Kind: "http", Name: "GET /clean", Fn: "svc.Clean"},
			{Kind: "http", Name: "GET /blind", Fn: "svc.Blind"},
		},
		BlindSpots: []graph.BlindSpot{{Kind: "reflect", Site: "svc.Blind", Detail: "reflective call"}},
	}

	rep := Build(base, branch)
	if len(rep.NewBlind) != 1 || rep.NewBlind[0].FQN != "svc.Blind" {
		t.Errorf("svc.Blind should be NEW BLIND (base had no blind spot): %+v", rep.NewBlind)
	}
	if len(rep.Accounted) != 1 || rep.Accounted[0].FQN != "svc.Clean" {
		t.Errorf("svc.Clean should be ACCOUNTED (resolved effect, no blind spot): %+v", rep.Accounted)
	}
	if len(rep.Carried) != 0 {
		t.Errorf("nothing should be carried here: %+v", rep.Carried)
	}

	md := rep.RenderMarkdown()
	if !strings.Contains(md, "db SELECT users") || !strings.Contains(md, "COMPLETE boundary-effect surface") {
		t.Errorf("accounted evidence (the resolved effect surface) not rendered:\n%s", md)
	}
	if !strings.Contains(md, "reflection") || !strings.Contains(md, "FLOOR") {
		t.Errorf("new-blind reason/floor not rendered:\n%s", md)
	}
	if !strings.Contains(md, "not approval") {
		t.Errorf("the accounted zone must state it is NOT approval:\n%s", md)
	}
}

// TestNewVsCarriedBlindness pins the diff-delta heart of the three-zone model: a
// pre-existing blind spot on an unchanged downstream node is CARRIED (not this MR's
// fault), while a blind spot the change NEWLY reaches (via an added edge) is NEW. The
// function that does both lands in NEW (new blindness dominates) and discloses the
// carried part too.
func TestNewVsCarriedBlindness(t *testing.T) {
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.A", Sig: "o"}, {FQN: "svc.B", Sig: "o"}, {FQN: "svc.deep"}},
		Edges: []graph.Edge{{From: "svc.A", To: "svc.deep"}, {From: "svc.B", To: "svc.deep"}},
		// deep is already blind on both A and B's paths in the base.
		BlindSpots: []graph.BlindSpot{{Kind: "reflect", Site: "svc.deep", Detail: "d"}},
	}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.A", Sig: "n"}, {FQN: "svc.B", Sig: "n2"}, {FQN: "svc.deep"}, {FQN: "svc.new"}},
		Edges: []graph.Edge{
			{From: "svc.A", To: "svc.deep"},
			{From: "svc.B", To: "svc.deep"},
			{From: "svc.B", To: "svc.new"}, // B newly reaches a newly-blind node
		},
		BlindSpots: []graph.BlindSpot{
			{Kind: "reflect", Site: "svc.deep", Detail: "d"}, // pre-existing
			{Kind: "reflect", Site: "svc.new", Detail: "x"},  // newly reachable from B
		},
	}

	rep := Build(base, branch)

	// A only carries the pre-existing deep blindness ⇒ Carried.
	if len(rep.Carried) != 1 || rep.Carried[0].FQN != "svc.A" {
		t.Errorf("svc.A should be CARRIED (deep was already blind in base): carried=%+v", rep.Carried)
	}
	// B newly reaches svc.new ⇒ New blind, but it also carries deep. (svc.new, a brand-new
	// blind function, is correctly NEW too — so locate B by name, don't assume the count.)
	var b *ChangedFn
	for i := range rep.NewBlind {
		if rep.NewBlind[i].FQN == "svc.B" {
			b = &rep.NewBlind[i]
		}
	}
	if b == nil {
		t.Fatalf("svc.B should be NEW BLIND (newly reaches svc.new): newBlind=%+v", rep.NewBlind)
	}
	if len(b.NewSeams) != 1 || b.NewSeams[0].Site != "svc.new" {
		t.Errorf("svc.B's NEW seam should be svc.new, got %+v", b.NewSeams)
	}
	if len(b.CarriedSeams) != 1 || b.CarriedSeams[0].Site != "svc.deep" {
		t.Errorf("svc.B should also carry the pre-existing svc.deep seam, got %+v", b.CarriedSeams)
	}
	if !strings.Contains(rep.RenderMarkdown(), "also passes through pre-existing blindness") {
		t.Errorf("a new-blind change that also carries blindness must disclose the carried part:\n%s", rep.RenderMarkdown())
	}
}

// TestSeverityTrivialStaysAccounted pins #2 under the three-zone model: a change whose
// only forward blind spot is producer-tagged "trivial" stays ACCOUNTED (not blind), with
// the benign seam disclosed so completeness is never over-claimed.
func TestSeverityTrivialStaysAccounted(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Benign", Sig: "old"}}}
	branch := &graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.Benign", Sig: "new"}},
		BlindSpots:  []graph.BlindSpot{{Kind: "ConcurrentDispatch", Site: "svc.Benign", Detail: "cancel func", Severity: "trivial"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /b", Fn: "svc.Benign"}},
	}
	rep := Build(base, branch)
	if len(rep.NewBlind) != 0 || len(rep.Carried) != 0 {
		t.Fatalf("a trivial seam must not be blind: new=%+v carried=%+v", rep.NewBlind, rep.Carried)
	}
	if len(rep.Accounted) != 1 || len(rep.Accounted[0].BenignSeams) != 1 {
		t.Fatalf("the benign seam must be accounted AND disclosed, got %+v", rep.Accounted)
	}
	if !strings.Contains(rep.RenderMarkdown(), "producer-tagged trivial") {
		t.Errorf("the set-aside benign seam was not disclosed:\n%s", rep.RenderMarkdown())
	}
}

// TestNewBlindRankedByConsequence pins #4: the new-blind zone orders the most
// consequential change first (critical tier ahead of low tier).
func TestNewBlindRankedByConsequence(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Low", Sig: "o"}, {FQN: "svc.Crit", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Low", Sig: "n", Tier: 3}, {FQN: "svc.Crit", Sig: "n", Tier: 1}},
		Edges: []graph.Edge{
			{From: "svc.Crit", To: "boundary:db INSERT ledger", Boundary: "outbound-sync"},
			{From: "svc.Low", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
		},
		BlindSpots: []graph.BlindSpot{
			{Kind: "reflect", Site: "svc.Crit", Detail: "c"},
			{Kind: "reflect", Site: "svc.Low", Detail: "l"},
		},
	}
	rep := Build(base, branch)
	if len(rep.NewBlind) != 2 {
		t.Fatalf("want 2 new-blind changes, got %+v", rep.NewBlind)
	}
	if rep.NewBlind[0].FQN != "svc.Crit" {
		t.Errorf("new-blind order = [%s, %s], want critical-tier first", rep.NewBlind[0].FQN, rep.NewBlind[1].FQN)
	}
}

// TestCallerBlindSpotDoesNotForceFocus pins #1: a clean change merely CALLED by
// reflective code stays accounted — the blind spot is in the caller (reverse reach),
// not in what the change can do.
func TestCallerBlindSpotDoesNotForceFocus(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.RefCaller", Sig: "o"}, {FQN: "svc.Clean", Sig: "n"}},
		Edges: []graph.Edge{
			{From: "svc.RefCaller", To: "svc.Clean"},
			{From: "svc.Clean", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
		},
		BlindSpots:  []graph.BlindSpot{{Kind: "reflect", Site: "svc.RefCaller", Detail: "upstream"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /x", Fn: "svc.RefCaller"}},
	}
	rep := Build(base, branch)
	found := false
	for _, cf := range rep.Accounted {
		if cf.FQN == "svc.Clean" {
			found = true
		}
	}
	if !found {
		t.Fatalf("svc.Clean must be ACCOUNTED despite a reflective caller; new=%+v carried=%+v accounted=%+v", rep.NewBlind, rep.Carried, rep.Accounted)
	}
}

// TestRenderMermaidZonesAndSafety pins the diagram: it declares all three zone classes,
// colors a new-blind change with its seam node, and entity-escapes the angle brackets a
// <dynamic> effect carries (an unescaped "<" would break the Mermaid parser).
func TestRenderMermaidZonesAndSafety(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "o"}, {FQN: "svc.Dyn", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "n", Tier: 2}, {FQN: "svc.Dyn", Sig: "n", Tier: 1}},
		Edges: []graph.Edge{
			{From: "svc.Clean", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
			{From: "svc.Dyn", To: "boundary:bus PUBLISH <dynamic>", Boundary: "outbound-async"},
		},
		BlindSpots: []graph.BlindSpot{{Kind: "NonConstantBoundaryArg", Site: "svc.Dyn", Detail: "non-const topic"}},
	}
	md := Build(base, branch).RenderMermaid()
	for _, want := range []string{"flowchart LR", "classDef newblind", "classDef carried", "classDef accounted", ":::newblind", ":::accounted"} {
		if !strings.Contains(md, want) {
			t.Errorf("mermaid missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "PUBLISH <dynamic>") || !strings.Contains(md, "&lt;dynamic&gt;") {
		t.Errorf("the <dynamic> effect label must be entity-escaped for Mermaid:\n%s", md)
	}
}

// TestBuildNoStructuralChange: identical graphs ⇒ nothing to triage, and the render says
// so explicitly rather than emitting a blank page (silence is never a silent pass).
func TestBuildNoStructuralChange(t *testing.T) {
	g := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.A", Sig: "s"}}}
	rep := Build(g, g)
	if len(rep.NewBlind)+len(rep.Carried)+len(rep.Accounted) != 0 {
		t.Fatalf("identical graphs must yield no changed functions, got %+v / %+v / %+v", rep.NewBlind, rep.Carried, rep.Accounted)
	}
	if !strings.Contains(rep.RenderMarkdown(), "No structural change detected") {
		t.Errorf("a no-change render must say so explicitly:\n%s", rep.RenderMarkdown())
	}
}
