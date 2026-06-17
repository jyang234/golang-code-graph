package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// l1Graph is the shared Phase-3 fixture: a discovered entry (handler) whose cone
// does NOT reach the effect; the effect is reached only through a SEVERED handler
// (Admin).Purge — a node present in the graph but reachable from no root (a missed
// edge into it). The emitter (store.del) sits behind Purge. An L1-tagged trace can
// localize PRECISELY to Purge, where L0 could only point at the entry.
func l1Graph() *graph.Index {
	return graph.NewIndex(&graph.Graph{
		Nodes: []graph.Node{
			{FQN: "ex.com/svc.handler"},        // discovered root; reaches no effect
			{FQN: "(*ex.com/svc.Admin).Purge"}, // severed handler; reaches the emitter
			{FQN: "ex.com/svc.del"},            // emitter function
		},
		Edges: []graph.Edge{
			{From: "(*ex.com/svc.Admin).Purge", To: "ex.com/svc.del"},
			{From: "ex.com/svc.del", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "ex.com/svc.handler"}},
	})
}

// l1Trace drives the severed Purge via an internal span carrying its RUNTIME FQN
// tag, which canonFQN reconciles to the ssa node "(*ex.com/svc.Admin).Purge".
func l1Trace() *ir.CanonicalTrace {
	return &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{
				Op: "internal purge", Kind: ir.KindInternal,
				Attrs: map[string]string{FQNTagKey: "ex.com/svc.(*Admin).Purge"},
				Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
					{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
				}}},
			},
		}}},
	}}
}

// TestLocalizeL1PreciseNodeSite is the headline Phase-3 result: with the internal
// span tagged, the walk localizes to the precise NODE on the observed path that is
// severed from every root and reaches the emitter — "(*ex.com/svc.Admin).Purge" —
// at Level L1, where the L0 walk could only have named the entry function. The
// CausalPath is disclosed, and the proof obligation holds.
func TestLocalizeL1PreciseNodeSite(t *testing.T) {
	ix := l1Graph()
	r := Audit("svc", ix, []*ir.CanonicalTrace{l1Trace()}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Severance == nil {
		t.Fatal("no severance")
	}
	if w.Severance.Level != LevelL1 {
		t.Errorf("Level = %q, want %q (the span carries a flowmap.fqn tag)", w.Severance.Level, LevelL1)
	}
	if w.Severance.Site != "(*ex.com/svc.Admin).Purge" {
		t.Errorf("Site = %q, want the precise severed node", w.Severance.Site)
	}
	if w.Severance.Kind != SeveranceSeveredEmitter {
		t.Errorf("Kind = %q, want %q", w.Severance.Kind, SeveranceSeveredEmitter)
	}
	// The disclosed causal path is the observed op chain, entry→effect.
	wantPath := []string{"HTTP POST /x", "internal purge", "DB postgres DELETE ledger"}
	if got := w.Observed.CausalPath; len(got) != 3 || got[0] != wantPath[0] || got[2] != wantPath[2] {
		t.Errorf("CausalPath = %v, want %v", got, wantPath)
	}
}

// TestLocalizeL1FallsBackToL0 confirms the level is a dial, not a premise: the
// SAME graph and candidate, driven by a trace WITHOUT the FQN tag, still localizes
// — at L0, to the coarse entry-function Site. Soundness is invariant; only the
// resolution changes (§7).
func TestLocalizeL1FallsBackToL0(t *testing.T) {
	ix := l1Graph()
	untagged := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "internal purge", Kind: ir.KindInternal, Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
			}}}},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{untagged}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(r.Candidates))
	}
	w := r.Candidates[0]
	if w.Severance.Level != LevelL0 {
		t.Errorf("Level = %q, want %q (no tag)", w.Severance.Level, LevelL0)
	}
	if w.Severance.Site != "ex.com/svc.handler" {
		t.Errorf("Site = %q, want the coarse entry function at L0", w.Severance.Site)
	}
}

// TestLocalizeL1AbsentFromGraphHint exercises the sharp signal (§7): an internal
// span whose FQN tag keys a function the graph has NO node for rides into the
// witness as the AbsentFromGraph hint, disclosed beside (never replacing) the
// walk's Site — the weak-at-L1, sharp-at-L2 localized missing node.
func TestLocalizeL1AbsentFromGraphHint(t *testing.T) {
	// Effect is unmodeled (no emitter node) and observed from a discovered entry
	// whose internal span names a ghost function.
	ix := graph.NewIndex(&graph.Graph{
		Nodes:       []graph.Node{{FQN: "ex.com/svc.handler"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "ex.com/svc.handler"}},
	})
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{
				Op: "internal ghost", Kind: ir.KindInternal,
				Attrs: map[string]string{FQNTagKey: "ex.com/svc.(*Ghost).Wipe"},
				Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
					{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
				}}},
			},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Severance.Level != LevelL1 {
		t.Errorf("Level = %q, want %q", w.Severance.Level, LevelL1)
	}
	if w.Severance.AbsentFromGraph != "ex.com/svc.(*Ghost).Wipe" {
		t.Errorf("AbsentFromGraph = %q, want the ghost tag", w.Severance.AbsentFromGraph)
	}
	// Kind is derived from the contradiction's shape (no emitter modeled, discovered
	// entry), NOT from the resolution level: an unmodeled effect is unmodeled-effect
	// at L1 exactly as it is at L0 — never the false 'severed-emitter' (which would
	// assert an emitter exists). This is the L0/L1 classifier parity.
	if w.Severance.Kind != SeveranceUnmodeledEffect {
		t.Errorf("Kind = %q, want %q (Kind must not depend on capture richness)", w.Severance.Kind, SeveranceUnmodeledEffect)
	}
}

// TestSeveranceKindLevelIndependent is the direct parity guard for the L0/L1
// classifier (§6/§7): the SAME unmodeled-effect candidate, audited once with an FQN
// tag (L1) and once without (L0), must carry the SAME Kind. Only Level and Site
// resolution may differ — never the classification, or the witness Kind would be a
// function of capture richness rather than the contradiction's shape.
func TestSeveranceKindLevelIndependent(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes:       []graph.Node{{FQN: "ex.com/svc.handler"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "ex.com/svc.handler"}},
	})
	mk := func(tagged bool) *ir.CanonicalSpan {
		inner := &ir.CanonicalSpan{Op: "internal work", Kind: ir.KindInternal,
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{{Op: "DB postgres DELETE ledger", Kind: ir.KindClient}}}}}
		if tagged {
			inner.Attrs = map[string]string{FQNTagKey: "ex.com/svc.(*Ghost).Wipe"}
		}
		return &ir.CanonicalSpan{Op: "HTTP POST /x", Kind: ir.KindServer,
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{inner}}}}
	}
	l0 := Audit("svc", ix, []*ir.CanonicalTrace{{Flow: "POST /x", Service: "svc", Root: mk(false)}}, Provenance{})
	l1 := Audit("svc", ix, []*ir.CanonicalTrace{{Flow: "POST /x", Service: "svc", Root: mk(true)}}, Provenance{})
	if len(l0.Candidates) != 1 || len(l1.Candidates) != 1 {
		t.Fatalf("want 1 candidate each, got L0=%d L1=%d", len(l0.Candidates), len(l1.Candidates))
	}
	if l0.Candidates[0].Severance.Level != LevelL0 || l1.Candidates[0].Severance.Level != LevelL1 {
		t.Fatalf("levels: L0=%q L1=%q", l0.Candidates[0].Severance.Level, l1.Candidates[0].Severance.Level)
	}
	if k0, k1 := l0.Candidates[0].Severance.Kind, l1.Candidates[0].Severance.Kind; k0 != k1 {
		t.Errorf("Kind differs by level: L0=%q L1=%q (must be level-independent)", k0, k1)
	}
}

// TestSelfExtinguishDryRun is the §6/§8 acceptance criterion at Phase 3: ratifying
// a blind_spot at the localized Site must EXTINGUISH the target impeachment (the
// emitter falls into the disclosed-seam cone, so it is RECLAIMED-LIVE, not a
// candidate) while creating NO new candidate — the MONOTONIC test (§13 crack #4),
// not "the count drops by exactly one". A localizer whose Site does not
// self-extinguish would be refused; this proves the L1 Site does.
func TestSelfExtinguishDryRun(t *testing.T) {
	ix := l1Graph()
	before := Audit("svc", ix, []*ir.CanonicalTrace{l1Trace()}, Provenance{})
	if len(before.Candidates) != 1 {
		t.Fatalf("want 1 candidate before repair, got %d", len(before.Candidates))
	}
	site := before.Candidates[0].Severance.Site
	if site != "(*ex.com/svc.Admin).Purge" {
		t.Fatalf("unexpected Site %q", site)
	}

	// Ratify the proposed repair: disclose a blind spot at the Site. (Phase 4 makes
	// this a human-ratified write; here it is the dry run.)
	g2 := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: "ex.com/svc.handler"},
			{FQN: "(*ex.com/svc.Admin).Purge"},
			{FQN: "ex.com/svc.del"},
		},
		Edges: []graph.Edge{
			{From: "(*ex.com/svc.Admin).Purge", To: "ex.com/svc.del"},
			{From: "ex.com/svc.del", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "ex.com/svc.handler"}},
		BlindSpots:  []graph.BlindSpot{{Site: site, Kind: "UnresolvedDispatch"}},
	}
	after := Audit("svc", graph.NewIndex(g2), []*ir.CanonicalTrace{l1Trace()}, Provenance{})

	// Target extinguished: the DELETE is no longer a candidate.
	for _, c := range after.Candidates {
		if c.Effect == "db DELETE ledger" {
			t.Errorf("repair did not extinguish the target impeachment: %+v", c)
		}
	}
	// Monotonic: no NEW candidate was created by the disclosure (proofs only weaken).
	if len(after.Candidates) > len(before.Candidates) {
		t.Errorf("repair created new candidates: before=%d after=%d", len(before.Candidates), len(after.Candidates))
	}
}

// TestLocalizeL1Deterministic pins P3's cross-cutting determinism (§10): the L1
// walk, the node index, and the causal-path threading keep the report a pure
// function of (graph, corpus) — byte-identical across runs and trace order.
func TestLocalizeL1Deterministic(t *testing.T) {
	ix := l1Graph()
	a := Audit("svc", ix, []*ir.CanonicalTrace{l1Trace(), l1Trace()}, Provenance{})
	b := Audit("svc", ix, []*ir.CanonicalTrace{l1Trace()}, Provenance{})
	if a.Digest != b.Digest {
		t.Errorf("L1 report not deterministic / duplicate-stable: %s != %s", a.Digest, b.Digest)
	}
}

// TestSeverancePathsDistinctByTag pins two coupled properties around path identity:
//   - two causal paths to one effect that share an OP chain but carry different
//     `flowmap.fqn` tags are NOT collapsed (pathSig folds the tag), so each path's
//     distinct localization survives instead of one being silently dropped; and
//   - the candidate order is a TOTAL order (lessWitness tie-breaks on the path), so
//     the non-stable candidate sort cannot order the two by trace arrival.
func TestSeverancePathsDistinctByTag(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes:       []graph.Node{{FQN: "ex.com/svc.handler"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "ex.com/svc.handler"}},
	})
	const tagA, tagB = "ex.com/svc.(*A).Do", "ex.com/svc.(*B).Do"
	// Two captures of the same flow whose internal span shares the op "internal
	// work" but carries a different FQN tag — two distinct causal paths to the one
	// effect key, differing ONLY in the tag.
	trace := func(tag string) *ir.CanonicalTrace {
		return &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
			Op: "HTTP POST /x", Kind: ir.KindServer,
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{{
				Op: "internal work", Kind: ir.KindInternal, Attrs: map[string]string{FQNTagKey: tag},
				Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{{Op: "DB postgres DELETE ledger", Kind: ir.KindClient}}}},
			}}}},
		}}
	}
	trA, trB := trace(tagA), trace(tagB)
	r := Audit("svc", ix, []*ir.CanonicalTrace{trA, trB}, Provenance{})
	if len(r.Candidates) != 2 {
		t.Fatalf("tag-distinguished paths collapsed: want 2 candidates, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	// The two carry the two distinct tags (no path's localization was dropped).
	hints := map[string]bool{r.Candidates[0].Severance.AbsentFromGraph: true, r.Candidates[1].Severance.AbsentFromGraph: true}
	if !hints[tagA] || !hints[tagB] {
		t.Errorf("paths not distinguished by tag: hints = %v", hints)
	}
	// lessWitness is a TOTAL order over them: exactly one direction is true (so the
	// non-stable candidate sort cannot order them by arrival).
	c0, c1 := r.Candidates[0], r.Candidates[1]
	if lessWitness(c0, c1) == lessWitness(c1, c0) {
		t.Error("lessWitness is not a total order: two distinct-path candidates compare equal (arrival-order dependent)")
	}
	// Order-independent digest across trace arrival order (the corpus is a set).
	if swapped := Audit("svc", ix, []*ir.CanonicalTrace{trB, trA}, Provenance{}); r.Digest != swapped.Digest {
		t.Errorf("digest depends on trace arrival order: %s != %s", r.Digest, swapped.Digest)
	}
}
