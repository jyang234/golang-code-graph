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

	// The new-blind change is promoted to a visible callout BEFORE the accounted <details>.
	iDyn := strings.Index(out, "svc.Dyn")
	iAcc := strings.Index(out, "Fully accounted")
	if iDyn < 0 || iAcc < 0 || iDyn > iAcc {
		t.Errorf("new-blind item must be visible and precede the accounted <details>:\n%s", out)
	}
	// The lead is plain-language ("N spots need judgment"), the lower zones fold into
	// <details>, and the runtime-dispatch seam is promoted to a ⚠️ callout.
	if !strings.Contains(out, "<details>") || !strings.Contains(out, "spot(s) need judgment") {
		t.Errorf("summary must lead with the judgment-call count and fold lower zones into <details>:\n%s", out)
	}
	if !strings.Contains(out, "choose their target at runtime") {
		t.Errorf("a runtime-dispatch seam must be promoted to a plain-language callout:\n%s", out)
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

// TestSummaryMaskingCallout pins the highest-value catch: when a verified effect
// DISAPPEARS (effects_removed) and a new ExternalBoundaryCall seam targets a known
// instrumentation wrapper of the same domain, the summary says the effect reads as
// "removed" but likely isn't — it moved behind the wrapper. The reviewer would otherwise
// have to hand-join the blind-spot list against the removed-effect list to see this.
func TestSummaryMaskingCallout(t *testing.T) {
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.OpenPostgres", Sig: "old"}},
		Edges: []graph.Edge{{From: "svc.OpenPostgres", To: "boundary:db postgres", Boundary: "outbound-sync"}},
	}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.OpenPostgres", Sig: "new"}},
		// The DB edge is gone (wrapped); a new external-boundary seam hands off to otelsql.
		BlindSpots: []graph.BlindSpot{{
			Kind: "ExternalBoundaryCall", Site: "svc.OpenPostgres",
			Detail: "otelsql.Open", Package: "github.com/XSAM/otelsql", Severity: "effect-bearing",
		}},
	}
	rep := Build(base, branch, nil)
	if len(rep.EffectsRemoved) != 1 || rep.EffectsRemoved[0] != "db postgres" {
		t.Fatalf("EffectsRemoved = %v, want [db postgres]", rep.EffectsRemoved)
	}
	out := rep.RenderSummary(Options{})
	if !strings.Contains(out, "reads as **removed**") || !strings.Contains(out, "otelsql") || !strings.Contains(out, "`db postgres`") {
		t.Errorf("masking callout (removed effect × instrumentation wrapper) not surfaced:\n%s", out)
	}
	// It is named a heuristic, never a proof.
	if !strings.Contains(out, "likely isn't") || !strings.Contains(out, "heuristic") {
		t.Errorf("masking must be worded as a heuristic, not a proof:\n%s", out)
	}
}

// TestSummaryRoutineAggregationFailLoud pins two rules at once: a known telemetry/cache
// handoff (statsy) is AGGREGATED into the routine line, while an UNKNOWN package is
// SURFACED as a callout, never folded into routine (the FR's fail-loud rule — hiding is
// the dangerous direction).
func TestSummaryRoutineAggregationFailLoud(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.SendMetric", Sig: "o"}, {FQN: "svc.CallMystery", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.SendMetric", Sig: "n"}, {FQN: "svc.CallMystery", Sig: "n"}},
		BlindSpots: []graph.BlindSpot{
			{Kind: "ExternalBoundaryCall", Site: "svc.SendMetric", Package: "github.com/acme/statsy", Severity: "effect-bearing"},
			{Kind: "ExternalBoundaryCall", Site: "svc.CallMystery", Package: "github.com/acme/mystery", Severity: "effect-bearing"},
		},
	}
	out := Build(base, branch, nil).RenderSummary(Options{})
	if !strings.Contains(out, "Routine — skim") || !strings.Contains(out, "`statsy`×1") {
		t.Errorf("a known telemetry handoff must aggregate into the routine line:\n%s", out)
	}
	if !strings.Contains(out, "hand off to a third-party package") || !strings.Contains(out, "CallMystery") {
		t.Errorf("an unknown package must be SURFACED as a callout (fail-loud), not hidden:\n%s", out)
	}
	// The unknown package must not be quietly swept into the routine roll-up.
	if strings.Contains(out, "`mystery`×") {
		t.Errorf("an unknown package must never appear in the routine aggregate:\n%s", out)
	}
}

// TestSummaryFoldsNotTruncates pins the fold-don't-truncate rule: when a callout caps its
// visible function list, every capped name still appears in the by-consequence <details>,
// so nothing is dropped from the record.
func TestSummaryFoldsNotTruncates(t *testing.T) {
	rep := Report{
		BaseNodes: 3, BranchNodes: 4,
		NewBlind: []ChangedFn{
			{FQN: "p.F0", NewSeams: []graph.BlindSpot{{Kind: "NonConstantBoundaryArg", Site: "p.F0"}}},
			{FQN: "p.F1", NewSeams: []graph.BlindSpot{{Kind: "NonConstantBoundaryArg", Site: "p.F1"}}},
			{FQN: "p.F2", NewSeams: []graph.BlindSpot{{Kind: "NonConstantBoundaryArg", Site: "p.F2"}}},
		},
	}
	out := rep.RenderSummary(Options{MaxNodes: 2})
	if !strings.Contains(out, "…+1 more") {
		t.Errorf("the callout function list must cap with a disclosed overflow:\n%s", out)
	}
	iDetails := strings.Index(out, "newly-blind function(s), by consequence")
	if iDetails < 0 || strings.Index(out, "F2") < iDetails {
		t.Errorf("the capped function must still be listed in the by-consequence <details>:\n%s", out)
	}
}

// TestScopePartitionsAuthoredFromDragged pins the core of --scope-fqns: a new-blind function
// the author EDITED leads (in scope, marked ✎), while one a changed callee dragged in (the
// author did not touch it and none of its seams sit at an authored site) folds into the
// dragged-in <details> — disclosed, never dropped.
func TestScopePartitionsAuthoredFromDragged(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Edited", Sig: "o"}, {FQN: "svc.Dragged", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Edited", Sig: "n"}, {FQN: "svc.Dragged", Sig: "n"}},
		BlindSpots: []graph.BlindSpot{
			{Kind: "NonConstantBoundaryArg", Site: "svc.Edited", Detail: "edited dyn"},
			{Kind: "NonConstantBoundaryArg", Site: "svc.Dragged", Detail: "dragged dyn"},
		},
	}
	rep := BuildScoped(base, branch, nil, []string{"svc.Edited"})
	if !rep.Scoped || len(rep.AuthoredScope) != 1 || rep.AuthoredScope[0] != "svc.Edited" {
		t.Fatalf("scope should be active and echo svc.Edited: %+v", rep)
	}
	out := rep.RenderSummary(Options{})
	iEdited := strings.Index(out, "Edited")
	iDragged := strings.Index(out, "Dragged in by a changed callee")
	if iEdited < 0 || iDragged < 0 || iEdited > iDragged {
		t.Errorf("the edited function must lead and the dragged-in <details> follow:\n%s", out)
	}
	// The dragged function appears only in the dragged-in section (below that summary), not
	// promoted to a lead callout.
	if strings.Index(out, "Dragged") < iDragged {
		t.Errorf("the dragged-in function must not be promoted to a lead callout:\n%s", out)
	}
	if !strings.Contains(out, "1 of them you edited directly") {
		t.Errorf("framing must report how many changed functions the author edited:\n%s", out)
	}
}

// TestScopeSeamLevelPromotesAuthoredCalleeViaCaller pins the seam-level soundness rule: when
// the author edits a callee with a body-only change (its signature is unchanged and it gains
// no out-edge, so it is NOT itself a "changed function"), the blindness it introduces
// surfaces only through a CALLER. That caller must be promoted (in scope) even though the
// author did not edit it — folding it would hide author-introduced blindness (fail-closed).
func TestScopeSeamLevelPromotesAuthoredCalleeViaCaller(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Caller", Sig: "o"}, {FQN: "svc.Callee", Sig: "same"}}}
	branch := &graph.Graph{
		// Caller gains an edge to Callee (so Caller is a changed function); Callee's sig is
		// UNCHANGED (a body-only edit), so Callee is not itself in the changed set.
		Nodes: []graph.Node{{FQN: "svc.Caller", Sig: "n"}, {FQN: "svc.Callee", Sig: "same"}},
		Edges: []graph.Edge{{From: "svc.Caller", To: "svc.Callee"}},
		// The blindness the author introduced lives at Callee, surfacing on Caller's cone.
		BlindSpots: []graph.BlindSpot{{Kind: "UnresolvedCall", Site: "svc.Callee", Detail: "new func value"}},
	}
	// The author edited Callee (body-only) — NOT Caller.
	rep := BuildScoped(base, branch, nil, []string{"svc.Callee"})
	out := rep.RenderSummary(Options{})
	if strings.Contains(out, "Dragged in by a changed callee") {
		t.Errorf("an authored seam must promote its caller, not fold it as dragged-in:\n%s", out)
	}
	if !strings.Contains(out, "can't resolve at all") || !strings.Contains(out, "↳ `svc.Caller`") {
		t.Errorf("the caller routed into the author's edited (blind) callee must be promoted and marked ↳:\n%s", out)
	}
}

// TestScopeMarkerAccuracyAndOrder pins two presentation properties: the ↳ badge ("a caller
// routed into your edit") is a SPECIFIC claim that must fire only on an actual authored-seam
// reach — a merely not-yours accounted function gets NO badge — and the author-edited
// functions sort first within the (demoted) accounted <details>.
func TestScopeMarkerAccuracyAndOrder(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Mine", Sig: "o"}, {FQN: "svc.Theirs", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Mine", Sig: "n"}, {FQN: "svc.Theirs", Sig: "n"}},
		Edges: []graph.Edge{
			{From: "svc.Mine", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
			{From: "svc.Theirs", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
		},
	}
	out := BuildScoped(base, branch, nil, []string{"svc.Mine"}).RenderSummary(Options{})
	// Both are accounted (no blind spots). Mine is ✎ and leads; Theirs gets no badge.
	iMine := strings.Index(out, "✎ `svc.Mine`")
	iTheirs := strings.Index(out, "`svc.Theirs`")
	if iMine < 0 || iTheirs < 0 || iMine > iTheirs {
		t.Errorf("the author-edited accounted function must be ✎ and sort first:\n%s", out)
	}
	if strings.Contains(out, "↳ `svc.Theirs`") {
		t.Errorf("a not-yours accounted function must not get a false ↳ badge:\n%s", out)
	}
}

// TestScopeFailLoudOnNoMatch pins the fail-loud rule: a scope set that matches NO branch
// function (an FQN-format slip) must NOT silently empty the review list — it falls back to
// the unscoped report and surfaces a loud caution.
func TestScopeFailLoudOnNoMatch(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.Real", Sig: "o"}}}
	branch := &graph.Graph{
		Nodes:      []graph.Node{{FQN: "svc.Real", Sig: "n"}},
		BlindSpots: []graph.BlindSpot{{Kind: "NonConstantBoundaryArg", Site: "svc.Real", Detail: "dyn"}},
	}
	rep := BuildScoped(base, branch, nil, []string{"wrong.Format", "also.Wrong"})
	if rep.Scoped {
		t.Fatalf("a zero-match scope set must leave scoping INACTIVE (fall back to unscoped)")
	}
	if rep.ScopeNote == "" {
		t.Fatalf("a zero-match scope set must surface a fail-loud note, got none")
	}
	out := rep.RenderSummary(Options{})
	if !strings.Contains(out, "showing UNSCOPED") {
		t.Errorf("the fail-loud caution must be visible in the summary:\n%s", out)
	}
	// The unscoped review list is intact — the real change is still surfaced.
	if !strings.Contains(out, "spot(s) need judgment") || strings.Contains(out, "you edited directly") {
		t.Errorf("fallback must show the UNSCOPED list (no scope framing):\n%s", out)
	}
}

// TestUnscopedJSONStable pins the non-goal: without --scope-fqns the new fields are absent
// from the report's JSON (omitempty), so existing machine consumers are unaffected.
func TestUnscopedJSONStable(t *testing.T) {
	base := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.A", Sig: "o"}}}
	branch := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.A", Sig: "n"}}}
	rep := Build(base, branch, nil)
	if rep.Scoped || rep.AuthoredScope != nil || rep.ScopeNote != "" {
		t.Errorf("an unscoped report must carry no scope state: %+v", rep)
	}
	for i := range rep.NewBlind {
		if rep.NewBlind[i].Authored {
			t.Errorf("no ChangedFn may be Authored without a scope set")
		}
	}
}

// TestMaskingIgnoresCarriedWrapper pins the fix for the worst masking bug: a PRE-EXISTING
// (carried) instrumentation wrapper must NOT be paired with a genuinely-dropped effect, or
// the highest-value catch fabricates a "still there, just instrumented" reassurance over a
// real removed dependency. Only a NEW wrapper seam can explain a removal.
func TestMaskingIgnoresCarriedWrapper(t *testing.T) {
	rep := Report{
		BaseNodes: 5, BranchNodes: 5,
		// The wrapper is CARRIED (pre-existed on base and branch), not introduced here.
		Carried: []ChangedFn{{
			FQN:          "svc.Conn",
			CarriedSeams: []graph.BlindSpot{{Kind: "ExternalBoundaryCall", Site: "svc.Conn", Package: "github.com/XSAM/otelsql"}},
		}},
		// An unrelated, genuine effect removal.
		EffectsRemoved: []string{"db postgres"},
	}
	if m := detectMasking(rep, nil); len(m) != 0 {
		t.Fatalf("a carried (pre-existing) wrapper must not produce a masking claim, got %+v", m)
	}
	if out := rep.RenderSummary(Options{}); strings.Contains(out, "reads as **removed**") {
		t.Errorf("a genuine removal must not be reframed as masking by a carried wrapper:\n%s", out)
	}
}

// TestOverApproxOnlyPromoted pins the fix for an over-approximation-only new-blind function
// (NewOverApprox, zero NewSeams): it must be promoted to a callout, not silently dropped into
// the folded details under a false "Nothing needs a judgment call" lead.
func TestOverApproxOnlyPromoted(t *testing.T) {
	rep := Report{
		BaseNodes: 3, BranchNodes: 4,
		NewBlind: []ChangedFn{{FQN: "svc.FanOut", Tier: 1, NewOverApprox: true}}, // no NewSeams
	}
	out := rep.RenderSummary(Options{})
	if strings.Contains(out, "Nothing in it needs a judgment call") {
		t.Errorf("an over-approx-only new-blind function must not trigger a 'nothing needs judgment' lead:\n%s", out)
	}
	if !strings.Contains(out, "UPPER BOUND") || !strings.Contains(out, "spot(s) need judgment") {
		t.Errorf("an over-approx-only new-blind function must be promoted to a callout:\n%s", out)
	}
}

// TestMaskingPerDomainGate pins the per-domain (not global) masking gate: a matched `db`
// masking callout must NOT suppress the fail-loud surfacing of an unmatched `http` wrapper.
func TestMaskingPerDomainGate(t *testing.T) {
	rep := Report{
		BaseNodes: 5, BranchNodes: 6,
		NewBlind: []ChangedFn{
			{FQN: "svc.OpenDB", NewSeams: []graph.BlindSpot{{Kind: "ExternalBoundaryCall", Site: "svc.OpenDB", Package: "github.com/XSAM/otelsql"}}},
			{FQN: "svc.CallAPI", NewSeams: []graph.BlindSpot{{Kind: "ExternalBoundaryCall", Site: "svc.CallAPI", Package: "go.opentelemetry.io/otelhttp"}}},
		},
		EffectsRemoved: []string{"db postgres"}, // matches db domain only
	}
	out := rep.RenderSummary(Options{})
	if !strings.Contains(out, "reads as **removed**") {
		t.Errorf("the db masking callout must fire:\n%s", out)
	}
	// The unmatched http wrapper must still surface as a masking-group callout (fail-loud).
	if !strings.Contains(out, "route through an instrumentation wrapper") || !strings.Contains(out, "CallAPI") {
		t.Errorf("a matched db callout must not suppress the unmatched http wrapper's surfacing:\n%s", out)
	}
}

// TestScopedMaskingNotAttributedToAuthor pins that, when scoped, a masking callout whose
// wrapper sits on a DRAGGED-IN (non-authored) function is surfaced but NOT counted as "in
// your changes" — it carries the dragged-in note and does not inflate the judgment count.
func TestScopedMaskingNotAttributedToAuthor(t *testing.T) {
	rep := Report{
		BaseNodes: 4, BranchNodes: 5,
		NewBlind: []ChangedFn{{
			FQN:      "svc.DraggedConn",
			NewSeams: []graph.BlindSpot{{Kind: "ExternalBoundaryCall", Site: "svc.DraggedConn", Package: "github.com/XSAM/otelsql"}},
		}},
		EffectsRemoved: []string{"db postgres"},
		Scoped:         true,
		AuthoredScope:  []string{"svc.SomethingElse"}, // the author edited something else
	}
	out := rep.RenderSummary(Options{})
	if !strings.Contains(out, "reads as **removed**") || !strings.Contains(out, "introduced by a changed callee") {
		t.Errorf("out-of-scope masking must surface AND be marked as not the author's edit:\n%s", out)
	}
	if strings.Contains(out, "spot(s) in your changes need judgment") {
		t.Errorf("out-of-scope masking must not be counted as 'in your changes':\n%s", out)
	}
}

// TestScopedEffectSurfaceIncludesRoutedCaller pins the seam-level effect surface: a
// ↳-promoted caller (in scope because a NEW seam sits at an authored callee) must contribute
// its reachable effects to the scoped 'reachable from code you edited' surface.
func TestScopedEffectSurfaceIncludesRoutedCaller(t *testing.T) {
	rep := Report{
		BaseNodes: 3, BranchNodes: 4,
		NewBlind: []ChangedFn{{
			FQN:      "svc.Caller", // NOT itself authored
			NewSeams: []graph.BlindSpot{{Kind: "UnresolvedCall", Site: "svc.Callee"}},
			Effects:  []string{"db INSERT ledger"},
		}},
		Scoped:        true,
		AuthoredScope: []string{"svc.Callee"}, // author edited the callee (the authored seam site)
	}
	out := rep.RenderSummary(Options{})
	if !strings.Contains(out, "reachable from code you edited") || !strings.Contains(out, "db INSERT ledger") {
		t.Errorf("a ↳-routed caller's effects must appear in the scoped effect surface:\n%s", out)
	}
}

// TestWrapperSegmentMatch pins that wrapper matching is "/"-segment-exact (like telemetry),
// so a coincidental package whose path merely CONTAINS a wrapper token is not misclassified.
func TestWrapperSegmentMatch(t *testing.T) {
	if got := instrWrapperToken("github.com/XSAM/otelsql"); got != "otelsql" {
		t.Errorf("a real wrapper segment must match: got %q", got)
	}
	if got := instrWrapperToken("github.com/acme/notelsql"); got != "" {
		t.Errorf("a coincidental substring must NOT match as a wrapper: got %q", got)
	}
	if classifySeam(graph.BlindSpot{Kind: "ExternalBoundaryCall", Package: "github.com/acme/notelsql"}) == classMasking {
		t.Errorf("notelsql must not classify as a masking wrapper")
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

// TestVerifiedDeltaEntrypoints pins BOTH halves of the entrypoint delta: a new route is
// reported as exposed, and a REMOVED route is reported (not silently dropped) — symmetric
// with the effect delta.
func TestVerifiedDeltaEntrypoints(t *testing.T) {
	base := &graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.H", Sig: "o"}, {FQN: "svc.Old", Sig: "o"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "DELETE /old", Fn: "svc.Old"}},
	}
	branch := &graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.H", Sig: "n"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /admin/ledger", Fn: "svc.H"}},
	}
	rep := Build(base, branch, nil)
	if len(rep.EntrypointsAdded) != 1 || rep.EntrypointsAdded[0] != "POST /admin/ledger" {
		t.Fatalf("EntrypointsAdded = %v, want [POST /admin/ledger]", rep.EntrypointsAdded)
	}
	if len(rep.EntrypointsRemoved) != 1 || rep.EntrypointsRemoved[0] != "DELETE /old" {
		t.Fatalf("EntrypointsRemoved = %v, want [DELETE /old] (a removed route must not be dropped)", rep.EntrypointsRemoved)
	}
	out := rep.RenderSummary(Options{})
	if !strings.Contains(out, "exposes 1 new entrypoint(s): `POST /admin/ledger`") || !strings.Contains(out, "removes 1 entrypoint(s): `DELETE /old`") {
		t.Errorf("summary must report both the new and the removed route:\n%s", out)
	}
}

// TestRouteIODeterministicMultiRoute pins the per-route ordering path (the one new ordering
// path in the per-route work): with a policy and several routes moving their write surface,
// the rendered summary is byte-identical across repeated runs — the rows arrive sorted on
// the intrinsic route FQN (via review.RouteIODeltas), not on the lossy display name.
func TestRouteIODeterministicMultiRoute(t *testing.T) {
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.A", Sig: "o"}, {FQN: "svc.B", Sig: "o"}},
		Edges: []graph.Edge{
			{From: "svc.A", To: "boundary:db SELECT x", Boundary: "outbound-sync"},
			{From: "svc.B", To: "boundary:db SELECT y", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /a", Fn: "svc.A"}, {Kind: "http", Name: "GET /b", Fn: "svc.B"}},
	}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.A", Sig: "n"}, {FQN: "svc.B", Sig: "n"}},
		Edges: []graph.Edge{
			{From: "svc.A", To: "boundary:db SELECT x", Boundary: "outbound-sync"},
			{From: "svc.A", To: "boundary:db INSERT audit", Boundary: "outbound-sync"},
			{From: "svc.B", To: "boundary:db SELECT y", Boundary: "outbound-sync"},
			{From: "svc.B", To: "boundary:db INSERT log", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "GET /a", Fn: "svc.A"}, {Kind: "http", Name: "GET /b", Fn: "svc.B"}},
	}
	p := &policy.Policy{Service: "svc"}
	if got := len(Build(base, branch, p).RouteIO); got != 2 {
		t.Fatalf("want 2 route moves, got %d", got)
	}
	want := Build(base, branch, p).RenderSummary(Options{})
	for i := 0; i < 6; i++ {
		if got := Build(base, branch, p).RenderSummary(Options{}); got != want {
			t.Fatalf("per-route render non-deterministic across runs:\n%s\n---\n%s", want, got)
		}
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
