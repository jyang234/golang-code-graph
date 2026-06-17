package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestSeveranceMissedRoot is the marquee Phase-2 localization: the impeachsvc
// missed admin route. The DB DELETE is observed from an entry the graph models no
// root for, so the severance walk localizes to the entry REGISTRATION literal
// (the seam, §6), classifies it missed-root with EntryDiscovered=false, and sorts
// it UNDISCLOSED (no frontier marker / blind spot covers it) — the high-value
// discovery the cell exists to find.
func TestSeveranceMissedRoot(t *testing.T) {
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	ix := graph.NewIndex(g)

	purge := loadTrace(t, impeachTraceAdminPurge)
	create := loadTrace(t, impeachTraceLoanCreate)
	r := Audit("impeachsvc", ix, []*ir.CanonicalTrace{purge, create}, Provenance{})

	if len(r.Candidates) != 1 {
		t.Fatalf("want exactly 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Severance == nil {
		t.Fatal("candidate carries no severance localization")
	}
	if w.Severance.Kind != SeveranceMissedRoot {
		t.Errorf("Severance.Kind = %q, want %q", w.Severance.Kind, SeveranceMissedRoot)
	}
	// The Site is the entry registration literal — the missed root IS the seam (§6).
	if w.Severance.Site != "HTTP DELETE /admin/ledger" {
		t.Errorf("Severance.Site = %q, want the entry registration literal", w.Severance.Site)
	}
	if w.Observed.EntryDiscovered {
		t.Error("EntryDiscovered = true, want false (the admin route is not a graph root)")
	}
	// Undisclosed: the seam is exactly what static did not know it had (§3). If a
	// future analyzer discloses it, this flips to known and the cell downgrades it.
	if w.Severance.FrontierKnown {
		t.Error("FrontierKnown = true, want false (the missed root is undisclosed)")
	}
	// The emitter the graph DID model (Purge) is the coarse anchor the missed-root
	// walk carries — the unreachable named effect behind the seam.
	if len(w.Severance.Anchors) != 1 ||
		w.Severance.Anchors[0] != "(*example.com/impeachsvc/internal/store.Loans).Purge" {
		t.Errorf("Anchors = %v, want the single unreachable emitter", w.Severance.Anchors)
	}
}

// TestSeveranceSeveredEmitter localizes the dispatch-seam flavor (§6): a
// discovered entry whose graph cone does NOT reach a modeled emitter. The break is
// upstream of the emitter, so the Site is the entry function (the upstream anchor
// L0 cannot resolve finer), and the proof obligation holds because the entry
// genuinely does not reach the emitter.
func TestSeveranceSeveredEmitter(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		// handler is the discovered root; orphan emits the effect but no edge
		// connects handler→orphan (the dispatch seam).
		Nodes: []graph.Node{{FQN: "svc.handler"}, {FQN: "svc.orphan"}},
		Edges: []graph.Edge{
			{From: "svc.orphan", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Severance == nil || w.Severance.Kind != SeveranceSeveredEmitter {
		t.Fatalf("Severance = %+v, want kind %q", w.Severance, SeveranceSeveredEmitter)
	}
	if !w.Observed.EntryDiscovered {
		t.Error("EntryDiscovered = false, want true (POST /x is a graph root)")
	}
	if w.Severance.Site != "svc.handler" {
		t.Errorf("Severance.Site = %q, want the discovered entry function (upstream anchor)", w.Severance.Site)
	}
	// L0 anchors: the entry function then the emitter — the coarse chain the Site
	// was derived from.
	want := []string{"svc.handler", "svc.orphan"}
	if len(w.Severance.Anchors) != len(want) {
		t.Fatalf("Anchors = %v, want %v", w.Severance.Anchors, want)
	}
	for i, a := range want {
		if w.Severance.Anchors[i] != a {
			t.Errorf("Anchors[%d] = %q, want %q", i, w.Severance.Anchors[i], a)
		}
	}
}

// TestSeveranceUnmodeledEffect localizes the absent flavor (§6): a discovered
// entry, but the graph models NO emitter for the observed effect. Static could not
// model or label the effect at all, so the Site is the entry function whose cone
// the effect escaped, and EntryDiscovered is true.
func TestSeveranceUnmodeledEffect(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.handler"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Claim.Reachability != ReachAbsent {
		t.Fatalf("Reachability = %q, want %q", w.Claim.Reachability, ReachAbsent)
	}
	if w.Severance == nil || w.Severance.Kind != SeveranceUnmodeledEffect {
		t.Fatalf("Severance = %+v, want kind %q", w.Severance, SeveranceUnmodeledEffect)
	}
	if !w.Observed.EntryDiscovered || w.Severance.Site != "svc.handler" {
		t.Errorf("want discovered entry svc.handler as Site; got discovered=%v site=%q",
			w.Observed.EntryDiscovered, w.Severance.Site)
	}
	// No emitter is modeled, so the anchor chain carries only the entry function.
	if len(w.Severance.Anchors) != 1 || w.Severance.Anchors[0] != "svc.handler" {
		t.Errorf("Anchors = %v, want [svc.handler] (no emitter to anchor)", w.Severance.Anchors)
	}
}

// TestSeveranceAbsentMissedRoot covers the doubly-severed case (§6): an unmodeled
// effect observed from an UNDISCOVERED entry. The missed root is the OUTERMOST
// seam encountered walking from the entry, so it wins the classification (the
// registration site is the first thing to repair), not unmodeled-effect.
func TestSeveranceAbsentMissedRoot(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{Nodes: []graph.Node{{FQN: "svc.handler"}}})
	tr := &ir.CanonicalTrace{Flow: "probe", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /unhinted", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Severance == nil || w.Severance.Kind != SeveranceMissedRoot {
		t.Fatalf("Severance = %+v, want kind %q (the missed root is the outer seam)", w.Severance, SeveranceMissedRoot)
	}
	if w.Observed.EntryDiscovered || w.Severance.Site != "HTTP POST /unhinted" {
		t.Errorf("want undiscovered entry as Site; got discovered=%v site=%q",
			w.Observed.EntryDiscovered, w.Severance.Site)
	}
	// No emitter modeled AND no entry mapped — the coarsest, empty anchor chain.
	if len(w.Severance.Anchors) != 0 {
		t.Errorf("Anchors = %v, want empty (no emitter, no mapped entry)", w.Severance.Anchors)
	}
}

// TestSeveranceProofObligation is the §6 fail-closed guard: when the observed
// effect IS statically reproducible from the observed entry, the "unreachable"
// claim was a mis-read — the walk finds no broken link, so it localizes to NO
// severance and discloses a self-inconsistency, never a fabricated seam. The
// candidate-generation path cannot produce this (a reached emitter is
// CONFIRMED-LIVE), so it is exercised directly through localize on a hand-built
// inconsistent witness.
func TestSeveranceProofObligation(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.handler"}, {FQN: "svc.store"}},
		Edges: []graph.Edge{
			{From: "svc.handler", To: "svc.store"},
			{From: "svc.store", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
	// A witness asserting unreachable for an effect the discovered entry plainly
	// reaches: the proof obligation must REFUSE to localize a seam.
	w := Witness{
		Effect:   "db DELETE ledger",
		Claim:    Claim{Reachability: ReachUnreachable},
		Observed: Observation{Flow: "POST /x", Service: "svc", Entry: "HTTP POST /x", Op: "DB postgres DELETE ledger"},
	}
	sev, discovered, ok := localize(w, ix)
	if ok {
		t.Fatal("proof obligation passed on a reproducible effect; want fail-closed")
	}
	if !discovered {
		t.Error("entry should still map to its discovered root")
	}
	if sev.Kind != SeveranceNone || sev.Site != "" {
		t.Errorf("Severance = %+v, want kind %q with no Site (never a fabricated seam)", sev, SeveranceNone)
	}
}

// TestSeveranceKnownFrontier is the known/unknown sort (§6): when a frontier
// marker already covers the severance Site, the cell sorts it KNOWN — behavior
// confirms a DISCLOSED seam, the lower-value "negative should have respected the
// frontier" case. (The undisclosed counterpart is TestSeveranceMissedRoot.)
func TestSeveranceKnownFrontier(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.handler"}, {FQN: "svc.orphan"}},
		Edges: []graph.Edge{
			{From: "svc.orphan", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
		// The graph discloses a frontier seam at the entry function: behavior then
		// confirms a KNOWN frontier, not an undisclosed blind spot.
		Frontier: &graph.FrontierSection{
			Markers: []graph.FrontierMarker{{Kind: "starved-entrypoint", Bin: "B", Site: "svc.handler"}},
		},
	})
	w := Witness{
		Effect:   "db DELETE ledger",
		Claim:    Claim{Reachability: ReachUnreachable},
		Observed: Observation{Flow: "POST /x", Service: "svc", Entry: "HTTP POST /x", Op: "DB postgres DELETE ledger"},
	}
	sev, _, ok := localize(w, ix)
	if !ok {
		t.Fatal("proof obligation failed on a genuinely severed emitter")
	}
	if sev.Site != "svc.handler" || !sev.FrontierKnown {
		t.Errorf("want Site svc.handler sorted KNOWN; got site=%q known=%v", sev.Site, sev.FrontierKnown)
	}
}

// TestSeveranceDeterministic pins the P2 cross-cutting requirement (§10): adding
// severance keeps the report a pure function of (graph, corpus) — byte-identical
// across runs and independent of trace arrival order, localization included.
func TestSeveranceDeterministic(t *testing.T) {
	ix := loadIndex(t, impeachsvcGraph)
	purge := loadTrace(t, impeachTraceAdminPurge)
	create := loadTrace(t, impeachTraceLoanCreate)

	a := Audit("impeachsvc", ix, []*ir.CanonicalTrace{purge, create}, Provenance{})
	b := Audit("impeachsvc", ix, []*ir.CanonicalTrace{create, purge}, Provenance{})
	if a.Digest != b.Digest {
		t.Errorf("severance broke order-independence: %s != %s", a.Digest, b.Digest)
	}
	if len(a.Candidates) != 1 || a.Candidates[0].Severance == nil {
		t.Fatalf("expected one localized candidate, got %+v", a.Candidates)
	}
}
