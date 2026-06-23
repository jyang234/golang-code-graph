package reviewtriage

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
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

	rep := Build(base, branch, nil)
	if len(rep.NewBlind) != 1 || rep.NewBlind[0].FQN != "svc.Blind" {
		t.Errorf("svc.Blind should be NEW BLIND (base had no blind spot): %+v", rep.NewBlind)
	}
	if len(rep.Accounted) != 1 || rep.Accounted[0].FQN != "svc.Clean" {
		t.Errorf("svc.Clean should be ACCOUNTED (resolved effect, no blind spot): %+v", rep.Accounted)
	}
	if len(rep.Carried) != 0 {
		t.Errorf("nothing should be carried here: %+v", rep.Carried)
	}

	md := rep.RenderMarkdown(Options{})
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

	rep := Build(base, branch, nil)

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
	if !strings.Contains(rep.RenderMarkdown(Options{}), "also passes through pre-existing blindness") {
		t.Errorf("a new-blind change that also carries blindness must disclose the carried part:\n%s", rep.RenderMarkdown(Options{}))
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
	rep := Build(base, branch, nil)
	if len(rep.NewBlind) != 0 || len(rep.Carried) != 0 {
		t.Fatalf("a trivial seam must not be blind: new=%+v carried=%+v", rep.NewBlind, rep.Carried)
	}
	if len(rep.Accounted) != 1 || len(rep.Accounted[0].BenignSeams) != 1 {
		t.Fatalf("the benign seam must be accounted AND disclosed, got %+v", rep.Accounted)
	}
	if !strings.Contains(rep.RenderMarkdown(Options{}), "producer-tagged trivial") {
		t.Errorf("the set-aside benign seam was not disclosed:\n%s", rep.RenderMarkdown(Options{}))
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
	rep := Build(base, branch, nil)
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
	rep := Build(base, branch, nil)
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
	md := Build(base, branch, nil).RenderMermaid(Options{})
	for _, want := range []string{"flowchart LR", "classDef newblind", "classDef carried", "classDef accounted", ":::newblind", ":::accounted"} {
		if !strings.Contains(md, want) {
			t.Errorf("mermaid missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "PUBLISH <dynamic>") || !strings.Contains(md, "&lt;dynamic&gt;") {
		t.Errorf("the <dynamic> effect label must be entity-escaped for Mermaid:\n%s", md)
	}
}

// TestScaleRollsUpAccountedNotNewBlind pins the scale invariant: over budget, the
// accounted bulk rolls up BY PACKAGE while the new-blind zone is never collapsed and the
// boundary-effect surface is never dropped; --full expands everything.
func TestScaleRollsUpAccountedNotNewBlind(t *testing.T) {
	rep := Report{
		BaseNodes: 10, BranchNodes: 12,
		NewBlind: []ChangedFn{{
			FQN: "pkg/a.Danger", Tier: 1,
			NewSeams: []graph.BlindSpot{{Kind: "reflect", Site: "pkg/a.Danger"}},
			Effects:  []string{"db INSERT t"},
		}},
		Accounted: []ChangedFn{
			{FQN: "example.com/svc/internal/handler.A", Effects: []string{"db SELECT users"}},
			{FQN: "example.com/svc/internal/handler.B", Effects: []string{"db SELECT users"}},
			{FQN: "example.com/svc/internal/store.C", Effects: []string{"db INSERT ledger"}},
			{FQN: "example.com/svc/internal/store.D", Effects: []string{"db INSERT ledger"}},
			{FQN: "example.com/svc/internal/store.E"},
		},
	}

	small := rep.RenderMermaid(Options{MaxNodes: 2})
	if !strings.Contains(small, "a.Danger") {
		t.Errorf("new-blind node must NEVER be collapsed:\n%s", small)
	}
	if !strings.Contains(small, "internal/handler · 2 accounted") || !strings.Contains(small, "internal/store · 3 accounted") {
		t.Errorf("accounted must roll up by package over budget:\n%s", small)
	}
	if strings.Contains(small, "handler.A") {
		t.Errorf("a rolled-up accounted zone must not emit per-function nodes:\n%s", small)
	}
	for _, e := range []string{"db SELECT users", "db INSERT ledger", "db INSERT t"} {
		if !strings.Contains(small, e) {
			t.Errorf("effect %q dropped under rollup — the I/O surface must be preserved:\n%s", e, small)
		}
	}

	full := rep.RenderMermaid(Options{Full: true})
	if !strings.Contains(full, "handler.A") {
		t.Errorf("--full must expand the accounted zone:\n%s", full)
	}

	md := rep.RenderMarkdown(Options{MaxNodes: 2})
	if !strings.Contains(md, "summarized by package") || strings.Contains(md, "handler.A") {
		t.Errorf("markdown accounted must summarize by package over budget:\n%s", md)
	}
	if !strings.Contains(md, "a.Danger") {
		t.Errorf("markdown new-blind must never be summarized:\n%s", md)
	}
}

// TestRenderSummary pins the MR-comment digest: new blindness is visible (the review
// list), carried and accounted are folded into <details>, effect labels are backtick-
// wrapped so a <dynamic> label is literal (not stray HTML), and the "not approval" caveat
// is present.
func TestRenderSummary(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "o"}, {FQN: "svc.Dyn", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "n", Tier: 2}, {FQN: "svc.Dyn", Sig: "n", Tier: 1}},
		Edges: []graph.Edge{
			{From: "svc.Clean", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
			{From: "svc.Dyn", To: "boundary:bus PUBLISH <dynamic>", Boundary: "outbound-async"},
		},
		BlindSpots: []graph.BlindSpot{{Kind: "NonConstantBoundaryArg", Site: "svc.Dyn", Detail: "non-const topic"}},
	}
	out := Build(base, branch, nil).RenderSummary(Options{})

	// The new-blind review item is visible and appears BEFORE the accounted <details>.
	iDyn := strings.Index(out, "svc.Dyn")
	iAcc := strings.Index(out, "Fully accounted")
	if iDyn < 0 || iAcc < 0 || iDyn > iAcc {
		t.Errorf("new-blind item must be visible and precede the accounted <details>:\n%s", out)
	}
	if !strings.Contains(out, "<details>") || !strings.Contains(out, "Review these") {
		t.Errorf("summary must lead with the review list and fold lower zones into <details>:\n%s", out)
	}
	// The <dynamic> effect must be inside a backtick span (literal), never raw HTML.
	if !strings.Contains(out, "`bus PUBLISH <dynamic>`") {
		t.Errorf("the <dynamic> effect must be backtick-wrapped so it renders literally:\n%s", out)
	}
	if !strings.Contains(out, "not approval") {
		t.Errorf("the accounted summary must state it is not approval:\n%s", out)
	}
	// The verified "what this MR does" delta: the base has no edges, so the branch ADDS both
	// boundary effects (and the <dynamic> one is backtick-wrapped here too).
	if !strings.Contains(out, "What this MR does (verified)") {
		t.Errorf("summary must include the verified what-it-does section:\n%s", out)
	}
	if !strings.Contains(out, "adds 2 external effect(s)") || !strings.Contains(out, "`db SELECT users`") {
		t.Errorf("verified delta must report the added effects:\n%s", out)
	}
}

// TestPerRouteWriteMovement pins the per-route refinement (reusing fitness.RouteWrites):
// a route whose surface moves from a read to a write is reported as "GET /x now writes …",
// displayed by route name, and only when a policy is supplied.
func TestPerRouteWriteMovement(t *testing.T) {
	base := &graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.GetX", Sig: "o"}},
		Edges:       []graph.Edge{{From: "svc.GetX", To: "boundary:db SELECT users", Boundary: "outbound-sync"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /x", Fn: "svc.GetX"}},
	}
	branch := &graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.GetX", Sig: "n"}},
		Edges:       []graph.Edge{{From: "svc.GetX", To: "boundary:db INSERT read_audit", Boundary: "outbound-sync"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /x", Fn: "svc.GetX"}},
	}

	rep := Build(base, branch, &policy.Policy{Service: "svc"})
	if len(rep.RouteIO) != 1 {
		t.Fatalf("want 1 route move, got %+v", rep.RouteIO)
	}
	if rm := rep.RouteIO[0]; rm.Route != "GET /x" || len(rm.Added) != 1 || rm.Added[0] != "db INSERT read_audit" {
		t.Fatalf("RouteMove = %+v, want `GET /x` now writes `db INSERT read_audit`", rm)
	}
	if !strings.Contains(rep.RenderSummary(Options{}), "`GET /x` now writes `db INSERT read_audit`") {
		t.Errorf("summary must show the per-route write movement:\n%s", rep.RenderSummary(Options{}))
	}

	// Without a policy the per-route section is skipped (the rest still works).
	if rep2 := Build(base, branch, nil); len(rep2.RouteIO) != 0 {
		t.Errorf("nil policy must skip per-route I/O, got %+v", rep2.RouteIO)
	}
}

// TestVerifiedDeltaEntrypoints pins the entrypoint half of the verified delta: a new route
// in the branch is reported as exposed.
func TestVerifiedDeltaEntrypoints(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.H", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.H", Sig: "n"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /admin/ledger", Fn: "svc.H"}},
	}
	rep := Build(base, branch, nil)
	if len(rep.EntrypointsAdded) != 1 || rep.EntrypointsAdded[0] != "POST /admin/ledger" {
		t.Fatalf("EntrypointsAdded = %v, want [POST /admin/ledger]", rep.EntrypointsAdded)
	}
	if !strings.Contains(rep.RenderSummary(Options{}), "exposes 1 new entrypoint(s): `POST /admin/ledger`") {
		t.Errorf("summary must report the new route:\n%s", rep.RenderSummary(Options{}))
	}
}

// TestRendersAreDeterministic pins CLAUDE.md's prime directive for the new ordering and
// emission paths: Build and both renders are pure functions of their inputs, byte-identical
// across repeated runs. It exercises the map-derived paths (rollupAccounted's grouping,
// distinctKinds, the Mermaid effect-id assignment) under several budgets, where a leaked
// map-iteration order would surface as a diff between two otherwise-identical runs.
func TestRendersAreDeterministic(t *testing.T) {
	rep := Report{
		BaseNodes: 6, BranchNodes: 10,
		NewBlind: []ChangedFn{
			{FQN: "x/p.A", Tier: 1, NewSeams: []graph.BlindSpot{{Kind: "reflect", Site: "x/p.A"}}, Effects: []string{"db INSERT t"}},
			{FQN: "x/q.B", Tier: 1, NewSeams: []graph.BlindSpot{{Kind: "DynamicEffect", Site: "x/q.B"}}, Effects: []string{"bus PUBLISH e"}},
		},
		Carried: []ChangedFn{{FQN: "x/p.C", CarriedSeams: []graph.BlindSpot{{Kind: "reflect", Site: "x/p.deep"}}}},
		Accounted: []ChangedFn{
			{FQN: "x/p.D", Effects: []string{"db SELECT u"}},
			{FQN: "x/p.E", Effects: []string{"db SELECT u"}},
			{FQN: "x/q.F", Effects: []string{"db INSERT t"}},
		},
	}
	for _, o := range []Options{{}, {Full: true}, {MaxNodes: 1}} {
		if a, b := rep.RenderMermaid(o), rep.RenderMermaid(o); a != b {
			t.Errorf("RenderMermaid non-deterministic at %+v:\n%s\n---\n%s", o, a, b)
		}
		if a, b := rep.RenderMarkdown(o), rep.RenderMarkdown(o); a != b {
			t.Errorf("RenderMarkdown non-deterministic at %+v", o)
		}
		if a, b := rep.RenderSummary(o), rep.RenderSummary(o); a != b {
			t.Errorf("RenderSummary non-deterministic at %+v", o)
		}
	}

	// Build itself: the changed-set and zone partition are map-derived; two runs over the
	// same graphs must produce the identical report.
	base := &graph.Graph{
		Nodes:      []graph.Node{{FQN: "x/p.A", Sig: "o"}, {FQN: "x/p.C", Sig: "o"}, {FQN: "x/p.deep"}},
		Edges:      []graph.Edge{{From: "x/p.C", To: "x/p.deep"}},
		BlindSpots: []graph.BlindSpot{{Kind: "reflect", Site: "x/p.deep"}},
	}
	branch := &graph.Graph{
		Nodes:      []graph.Node{{FQN: "x/p.A", Sig: "n"}, {FQN: "x/p.C", Sig: "n"}, {FQN: "x/p.deep"}, {FQN: "x/q.B", Sig: "n"}},
		Edges:      []graph.Edge{{From: "x/p.C", To: "x/p.deep"}, {From: "x/q.B", To: "boundary:db INSERT t", Boundary: "outbound-sync"}},
		BlindSpots: []graph.BlindSpot{{Kind: "reflect", Site: "x/p.deep"}},
	}
	if a, b := Build(base, branch, nil).RenderMarkdown(Options{}), Build(base, branch, nil).RenderMarkdown(Options{}); a != b {
		t.Errorf("Build+RenderMarkdown non-deterministic across runs:\n%s\n---\n%s", a, b)
	}
}

// TestMmLabelEscapesAmpersand pins finding #1: a label carrying '&' is entity-escaped
// (an unescaped '&' starts a Mermaid HTML entity and corrupts the node), matching the
// producer-side escaper.
func TestMmLabelEscapesAmpersand(t *testing.T) {
	if got := mmLabel("a & b"); got != "a &amp; b" {
		t.Errorf("mmLabel(%q) = %q, want '&' entity-escaped", "a & b", got)
	}
	if got := mmLabel("x\ny"); got != "x y" {
		t.Errorf("mmLabel must fold newlines to spaces, got %q", got)
	}
}

// TestBuildNoStructuralChange: identical graphs ⇒ nothing to triage, and the render says
// so explicitly rather than emitting a blank page (silence is never a silent pass).
func TestBuildNoStructuralChange(t *testing.T) {
	g := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.A", Sig: "s"}}}
	rep := Build(g, g, nil)
	if len(rep.NewBlind)+len(rep.Carried)+len(rep.Accounted) != 0 {
		t.Fatalf("identical graphs must yield no changed functions, got %+v / %+v / %+v", rep.NewBlind, rep.Carried, rep.Accounted)
	}
	if !strings.Contains(rep.RenderMarkdown(Options{}), "No structural change detected") {
		t.Errorf("a no-change render must say so explicitly:\n%s", rep.RenderMarkdown(Options{}))
	}
}
