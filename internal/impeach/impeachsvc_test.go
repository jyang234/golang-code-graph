package impeach

import (
	"slices"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// The impeachsvc fixture is a CONTROLLED environment for the impeachment cell: a
// real service whose graph the real analyzer produces, plus traces the real
// harness captures. Its admin route is registered through a custom (unhinted)
// router, so its DB DELETE is a MISSED ROOT — present in the graph, reachable
// from no discovered entrypoint, and DISCLOSED BY NOTHING (no blind spot, no
// frontier marker). That is the genuine analyzer blind spot the cell must catch,
// and the seam this whole fixture exists to manufacture inside a boundary we
// control independent of the test.
const (
	impeachsvcGraph         = "testdata/impeachsvc.graph.json"
	impeachTraceAdminPurge  = "../../testdata/fixtures/impeachsvc/flows/testdata/flows/delete_admin_ledger.golden.json"
	impeachTraceLoanCreate  = "../../testdata/fixtures/impeachsvc/flows/testdata/flows/post_loan.golden.json"
	impeachTraceAdminNotify = "../../testdata/fixtures/impeachsvc/flows/testdata/flows/post_admin_notify.golden.json"
)

// TestImpeachsvcCatchesUndisclosedMissedRoot is the end-to-end proof: the real
// graph + the two real captured traces, joined, yield EXACTLY ONE impeachment —
// the DB DELETE on the missed admin route — while the discovered route's DB
// INSERT is CONFIRMED-LIVE (neither impeached nor an unobserved gap). This is the
// cell catching a real, undisclosed false-negative end to end, not a synthetic
// stub.
func TestImpeachsvcCatchesUndisclosedMissedRoot(t *testing.T) {
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	ix := graph.NewIndex(g)

	// Precondition: the seam is genuinely UNDISCLOSED. If a future analyzer learns
	// to disclose this (a frontier marker or blind spot at the missed route), the
	// cell would correctly downgrade it to RECLAIMED-LIVE — so this assertion is
	// what keeps the fixture honest about what it is testing.
	if bs := ix.BlindSpots(); len(bs) != 0 {
		t.Fatalf("fixture no longer undisclosed: graph carries blind spots %+v", bs)
	}
	if fr := ix.Frontier(); fr != nil && len(fr.Markers) != 0 {
		t.Fatalf("fixture no longer undisclosed: graph carries frontier markers %+v", fr.Markers)
	}

	purge := loadTrace(t, impeachTraceAdminPurge)
	create := loadTrace(t, impeachTraceLoanCreate)
	r := Audit("impeachsvc", ix, []*ir.CanonicalTrace{purge, create}, Provenance{})

	// Determinism over the real corpus, now with MORE THAN ONE candidate: the
	// missed admin route reaches two named DELETEs, so this is the first real corpus
	// whose witness sort orders multiple findings. The digest is order-independent
	// AND the candidate SEQUENCE is identical regardless of input-trace order — the
	// sort resolves on intrinsic data (effect first), never on arrival order.
	rev := Audit("impeachsvc", ix, []*ir.CanonicalTrace{create, purge}, Provenance{})
	if r.Digest != rev.Digest {
		t.Error("report not order-independent over the real corpus")
	}
	wantOrder := []string{"db DELETE audit_log", "db DELETE ledger"} // lexical by effect (§5 tie-break)
	if got := effectsOf(r); !slices.Equal(got, wantOrder) {
		t.Errorf("candidate order = %v, want %v (deterministic sort on intrinsic effect key)", got, wantOrder)
	}
	if got := effectsOf(rev); !slices.Equal(got, wantOrder) {
		t.Errorf("reversed-input candidate order = %v, want %v (order-independent witness sort)", got, wantOrder)
	}

	if len(r.Candidates) != 2 {
		t.Fatalf("want exactly 2 impeachment candidates (ledger + audit_log), got %d: %+v", len(r.Candidates), r.Candidates)
	}
	// Both missed-route effects are impeached; inspect the ledger one as the witness
	// detail exemplar (the audit_log candidate is structurally identical bar the table).
	w := candidateFor(t, r, "db DELETE ledger")
	if w.Claim.Reachability != ReachUnreachable {
		t.Errorf("Reachability = %q, want %q (named effect, no discovered route reaches it)", w.Claim.Reachability, ReachUnreachable)
	}
	// The genuine contradiction is found, but this real corpus carries no code
	// identity (the trace model has no commit stamp yet, §14-D), so the ladder
	// caps it at VERSION-SKEW — fail-closed, not promoted to a bare IMPEACHMENT on
	// a graph it cannot prove the trace ran. The promotion is exercised separately
	// (TestImpeachsvcLadderPromotesWithProvenance) once the substrate is supplied.
	for _, c := range r.Candidates {
		if c.Verdict != DowngradeVersionSkew {
			t.Errorf("Verdict = %q for %q, want %q (real corpus has no code identity)", c.Verdict, c.Effect, DowngradeVersionSkew)
		}
	}
	// The witness carries the runtime counterexample: the entry it was reached
	// from (the missed admin route) and the enriched observed op (with DB system).
	if w.Observed.Entry != "HTTP DELETE /admin/ledger" {
		t.Errorf("Observed.Entry = %q, want the missed admin route", w.Observed.Entry)
	}
	if w.Observed.Op != "DB postgres DELETE ledger" {
		t.Errorf("Observed.Op = %q, want the enriched DB op", w.Observed.Op)
	}

	// The discovered route's INSERT must be CONFIRMED-LIVE: observed AND reachable,
	// so it is neither impeached nor reported as an unexercised coverage gap.
	for _, c := range r.Candidates {
		if c.Effect == "db INSERT loans" {
			t.Error("the discovered route's effect was wrongly impeached")
		}
	}
	for _, gap := range r.CoverageGaps {
		if gap == "db INSERT loans" {
			t.Error("the discovered route's effect read as unobserved despite being driven")
		}
	}
}

// TestImpeachsvcDiscoveredRouteAloneIsClean is the discrimination control: the
// discovered route's flow ON ITS OWN yields zero candidates. The cell fires on
// the missed route, not on the sound one.
func TestImpeachsvcDiscoveredRouteAloneIsClean(t *testing.T) {
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	r := Audit("impeachsvc", graph.NewIndex(g), []*ir.CanonicalTrace{loadTrace(t, impeachTraceLoanCreate)}, Provenance{})
	if len(r.Candidates) != 0 {
		t.Errorf("discovered route alone produced candidates: %+v", r.Candidates)
	}
}

// TestImpeachsvcCatchesBusPublishMissedRoot is the bus-vocabulary axis: the missed
// admin route reaches a constant-named bus PUBLISH (not a DB effect), so this proves
// the cell impeaches over the PUBLISH label vocabulary too — the label rung is not
// DB-specific. Without provenance it caps at VERSION-SKEW (fail-closed, like every
// stampless real capture); with the gated stamp + the corpus-carried integration
// grade it promotes to a true IMPEACHMENT, localized L1 to the severed Notify node.
func TestImpeachsvcCatchesBusPublishMissedRoot(t *testing.T) {
	const notifyNode = "(*example.com/impeachsvc/internal/admin.Admin).Notify"
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	notify := loadTrace(t, impeachTraceAdminNotify)

	// Stampless: a real captured bus effect is a genuine contradiction the ladder
	// caps at VERSION-SKEW until code identity is supplied — never a bare IMPEACHMENT.
	bare := Audit("impeachsvc", graph.NewIndex(g), []*ir.CanonicalTrace{notify}, Provenance{})
	w := candidateFor(t, bare, "PUBLISH ledger.purged")
	if w.Claim.Reachability != ReachUnreachable {
		t.Errorf("Reachability = %q, want %q (named bus effect, no discovered route reaches it)", w.Claim.Reachability, ReachUnreachable)
	}
	if w.Verdict != DowngradeVersionSkew {
		t.Errorf("Verdict = %q, want %q (stampless real capture)", w.Verdict, DowngradeVersionSkew)
	}
	if w.Observed.Op != "PUBLISH ledger.purged" {
		t.Errorf("Observed.Op = %q, want the canonical PUBLISH op", w.Observed.Op)
	}
	if w.Severance == nil || w.Severance.Level != LevelL1 || w.Severance.Site != notifyNode {
		t.Errorf("want L1 localization to the severed Notify node, got %+v", w.Severance)
	}

	// With provenance the bus PUBLISH promotes to a full IMPEACHMENT — the label rung
	// clears on the bus vocabulary exactly as it does for DB.
	g.Stamp = "deadbeefcafe"
	promoted := Audit("impeachsvc", graph.NewIndex(g), []*ir.CanonicalTrace{notify}, Provenance{TraceIdentity: "deadbeefcafe"})
	pw := candidateFor(t, promoted, "PUBLISH ledger.purged")
	if pw.Verdict != VerdictImpeachment {
		t.Fatalf("Verdict = %q, want %q; ladder = %+v", pw.Verdict, VerdictImpeachment, pw.Rungs)
	}
	for _, rung := range pw.Rungs {
		if rung.Name == RungLabel && !rung.Passed {
			t.Errorf("label rung must clear on the bus PUBLISH vocabulary: %s", rung.Evidence)
		}
	}
}

// TestImpeachsvcLadderPromotesWithProvenance is the end-to-end IMPEACHMENT proof:
// the SAME real graph + real captured traces that downgrade to VERSION-SKEW above
// promote to a true IMPEACHMENT once the capture-side substrate the ladder needs
// is supplied — a graph stamped with the deployed commit and a matching
// production capture identity. Every rung clears: static asserts a real negative,
// the graph is the stamped code the trace ran, the effect is in the one-source DB
// vocabulary, it is on impeachsvc's own spans, and the capture is production. This
// is the §10 go/no-go top rung — the ladder can reach IMPEACHMENT, so it is not a
// detector that only ever downgrades.
func TestImpeachsvcLadderPromotesWithProvenance(t *testing.T) {
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	// The committed corpus is stampless, so the caller injects the gated commit
	// identity; the capture grade comes from the corpus itself (the goldens carry
	// "integration", set by the harness producer, §12.6 — no caller assertion).
	const commit = "deadbeefcafe"
	g.Stamp = commit
	ix := graph.NewIndex(g)

	purge := loadTrace(t, impeachTraceAdminPurge)
	create := loadTrace(t, impeachTraceLoanCreate)
	prov := Provenance{TraceIdentity: commit}
	r := Audit("impeachsvc", ix, []*ir.CanonicalTrace{purge, create}, prov)

	if r.Digest != Audit("impeachsvc", ix, []*ir.CanonicalTrace{create, purge}, prov).Digest {
		t.Error("report not order-independent under provenance")
	}
	// Both missed-route DELETEs promote: the promotion path is exercised over MORE
	// THAN ONE finding, so "the ladder can reach IMPEACHMENT" is proven for each.
	if len(r.Candidates) != 2 {
		t.Fatalf("want exactly 2 candidates (ledger + audit_log), got %d: %+v", len(r.Candidates), r.Candidates)
	}
	wantOrder := []string{RungStaticAssertsNoPath, RungCodeIdentity, RungLabel, RungServiceScope, RungCaptureFidelity}
	for _, w := range r.Candidates {
		if w.Verdict != VerdictImpeachment {
			t.Fatalf("Verdict = %q for %q, want %q; ladder = %+v", w.Verdict, w.Effect, VerdictImpeachment, w.Rungs)
		}
		// The ladder is recorded WHOLE and every rung cleared (Passed == benign
		// explanation ruled out): an IMPEACHMENT is exactly "all five rungs passed".
		if len(w.Rungs) != 5 {
			t.Fatalf("want the full 5-rung ladder recorded for %q, got %d: %+v", w.Effect, len(w.Rungs), w.Rungs)
		}
		for i, rung := range w.Rungs {
			if rung.Name != wantOrder[i] {
				t.Errorf("rung %d = %q, want %q (ladder must be in §4 order)", i, rung.Name, wantOrder[i])
			}
			if !rung.Passed {
				t.Errorf("rung %q did not pass on the promotion path for %q: %s", rung.Name, w.Effect, rung.Evidence)
			}
		}
	}
	// Provenance is recorded in the report header (the numerator's identity, §5):
	// the injected code identity and the corpus-carried capture grade.
	if r.TraceIdentity != commit || r.CaptureProvenance != CaptureIntegration {
		t.Errorf("report dropped provenance: identity=%q capture=%q", r.TraceIdentity, r.CaptureProvenance)
	}
}
