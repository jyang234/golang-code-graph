package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/ir"
)

// The impeachsvc node FQNs (ssa spelling), pinned so a graph-shape change that
// moved the seam surfaces here rather than silently passing.
const (
	purgeLedgerNode = "(*example.com/impeachsvc/internal/admin.Admin).PurgeLedger"
	purgeEmitter    = "(*example.com/impeachsvc/internal/store.Loans).Purge"
	createHandler   = "(*example.com/impeachsvc/internal/handler.App).Create"
)

// e2eFixture loads the REAL impeachsvc graph (stamped, as CI would stamp the gated
// commit) and the REAL harness-captured corpus, and audits under production
// provenance — so the missed-root DB DELETE promotes to a true IMPEACHMENT,
// localized at L1 (the captured corpus now carries flowmap.fqn waypoint tags) to
// the precise severed node. This is the whole toolchain end to end: real analyzer
// graph, real captured behavior, real producer.
func e2eFixture(t *testing.T) (*graph.Index, Report, Provenance) {
	t.Helper()
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	const commit = "deadbeefcafe"
	g.Stamp = commit // the gated commit identity CI would pass via --stamp
	ix := graph.NewIndex(g)
	// The committed corpus is stampless (identity injected) but carries its capture
	// grade ("integration", set by the harness producer, §12.6) — so the caller
	// injects only the code identity and the grade comes from the corpus itself.
	prov := Provenance{TraceIdentity: commit}
	r := Audit("impeachsvc", ix, []*ir.CanonicalTrace{loadTrace(t, impeachTraceAdminPurge), loadTrace(t, impeachTraceLoanCreate)}, prov)
	// The missed route reaches two named DELETEs, so the real corpus carries TWO
	// IMPEACHMENTs — both localized L1 to the SAME severed node (PurgeLedger is the
	// missed root behind both effects). The single-witness downstream tests select
	// the ledger candidate by effect.
	if len(r.Candidates) != 2 {
		t.Fatalf("want two IMPEACHMENTs over the real corpus, got %+v", r.Candidates)
	}
	for _, w := range r.Candidates {
		if w.Verdict != VerdictImpeachment {
			t.Fatalf("Verdict = %q for %q, want IMPEACHMENT", w.Verdict, w.Effect)
		}
		if got := w.Severance; got == nil || got.Level != LevelL1 || got.Site != purgeLedgerNode {
			t.Fatalf("want L1 localization to the severed node for %q, got %+v", w.Effect, got)
		}
	}
	return ix, r, prov
}

// TestImpeachsvcCallerCaptureContradictsCorpus is the §12.6 close: the corpus
// self-describes its grade ("integration", set by the harness producer), so an
// audit caller asserting a CONTRADICTING grade ("production") fails closed —
// resolveCaptureProvenance returns "" and the capture-fidelity rung caps the
// candidate at CAPTURE-UNTRUSTED. The audit can no longer assert a grade the
// capture itself contradicts; a test corpus can never be laundered into production.
func TestImpeachsvcCallerCaptureContradictsCorpus(t *testing.T) {
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	g.Stamp = "deadbeefcafe"
	ix := graph.NewIndex(g)
	traces := []*ir.CanonicalTrace{loadTrace(t, impeachTraceAdminPurge), loadTrace(t, impeachTraceLoanCreate)}
	// Caller claims production; the corpus carries integration ⇒ contradiction.
	r := Audit("impeachsvc", ix, traces, Provenance{TraceIdentity: "deadbeefcafe", Capture: CaptureProduction})
	if len(r.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(r.Candidates))
	}
	// The fail-closed cap applies to EVERY candidate the contradicted grade touches.
	for _, w := range r.Candidates {
		if w.Verdict != DowngradeCaptureUntrusted {
			t.Errorf("Verdict = %q for %q, want %q (caller grade contradicts the corpus ⇒ fail closed)", w.Verdict, w.Effect, DowngradeCaptureUntrusted)
		}
	}
	if r.CaptureProvenance != "" {
		t.Errorf("resolved capture = %q, want \"\" (unestablished on contradiction)", r.CaptureProvenance)
	}
}

// TestImpeachsvcSelfExtinguishesE2E is the Phase-4 acceptance gate run end to end
// over the real fixture: the proposed blind-spot repair at the L1-localized severed
// node extinguishes the impeachment, monotonically (no proof newly created). This
// is the independent-grader evidence (tenet 5) that the loop's repairs resolve
// their own impeachments on REAL captures, not just in-memory unit fixtures.
func TestImpeachsvcSelfExtinguishesE2E(t *testing.T) {
	ix, r, prov := e2eFixture(t)
	w := candidateFor(t, r, "db DELETE ledger")
	rep := ProposeRepair(w)
	if rep == nil || rep.Kind != RepairBlindSpot || rep.Site != purgeLedgerNode {
		t.Fatalf("repair = %+v, want a blind_spot at the severed node", rep)
	}
	// A must_not_reach rule that holds statically must keep holding after the
	// repair (blinding adds disclosure, removes no edge) — the monotonic check over
	// real findings.
	p := &policy.Policy{Service: "impeachsvc", MustNotReach: []policy.ReachRule{
		{Name: "admin-deletes", From: []string{"(*example.com/impeachsvc/internal/admin.Admin)"}, To: []string{"boundary:db DELETE ledger"}},
	}}
	traces := []*ir.CanonicalTrace{loadTrace(t, impeachTraceAdminPurge), loadTrace(t, impeachTraceLoanCreate)}
	res := SelfExtinguishes(ix, rep, w, traces, prov, p, "impeachsvc")
	if !res.OK {
		t.Fatalf("real-fixture repair rejected: %+v", res)
	}
	if !res.Extinguished || !res.Monotonic {
		t.Errorf("want extinguished+monotonic over the real graph: %+v", res)
	}
}

// TestImpeachsvcViolatedE2E proves the §9 VIOLATED path end to end: a must_not_reach
// rule whose `to` binds the impeached effect and whose `from` binds a node on the
// witnessed causal path is a behaviorally-confirmed breach. It blocks on the
// committed corpus and is audit-only (no blocker) on a live one — the prime-directive
// fence (§13 crack #2), exercised over real captures.
func TestImpeachsvcViolatedE2E(t *testing.T) {
	ix, r, prov := e2eFixture(t)
	_ = prov
	rules := []policy.ReachRule{
		{Name: "no-admin-delete", From: []string{"(*example.com/impeachsvc/internal/admin.Admin)"}, To: []string{"boundary:db DELETE ledger"}},
	}
	committed := Resolve(r, ix, rules, OriginCommitted)
	// The rule's `to` binds only DELETE ledger, so that candidate upgrades to VIOLATED
	// while the audit_log candidate stays a bare IMPEACHMENT — so exactly ONE blocker,
	// the ledger breach (a bare impeachment never gates).
	w := candidateFor(t, committed.Report, "db DELETE ledger")
	if w.Verdict != VerdictViolated {
		t.Fatalf("Verdict = %q, want %q (witnessed breach from a path node)", w.Verdict, VerdictViolated)
	}
	if got := committed.GateBlockers(); len(got) != 1 || got[0].Verdict != VerdictViolated {
		t.Fatalf("committed corpus: want 1 VIOLATED blocker, got %+v", got)
	}
	if got := Resolve(r, ix, rules, OriginLive).GateBlockers(); len(got) != 0 {
		t.Errorf("live corpus must not gate: %+v", got)
	}
}

// TestImpeachsvcRequireProofDowngradeE2E proves the bare-impeachment gate path: a
// must_not_reach rule from the DISCOVERED web handler to the DELETE is SATISFIED
// statically (no discovered route reaches it — the missed root), but the
// behavioral witness impeaches that proof, downgrading it SATISFIED→CANT-PROVE. It
// blocks only under require_proof (fails closed on an unprovable invariant), and is
// advisory otherwise — exactly fitness's reach semantics, driven by behavior.
func TestImpeachsvcRequireProofDowngradeE2E(t *testing.T) {
	ix, r, _ := e2eFixture(t)
	// from = the discovered handler, which does NOT bind the witnessed path node
	// (PurgeLedger), so the impeachment stays BARE (not a VIOLATED).
	bare := []policy.ReachRule{{Name: "routes-no-delete", From: []string{createHandler}, To: []string{"boundary:db DELETE ledger"}}}
	if got := Resolve(r, ix, bare, OriginCommitted).GateBlockers(); len(got) != 0 {
		t.Errorf("advisory bare impeachment blocked: %+v", got)
	}

	strict := []policy.ReachRule{{Name: "routes-no-delete", From: []string{createHandler}, To: []string{"boundary:db DELETE ledger"}, RequireProof: true}}
	res := Resolve(r, ix, strict, OriginCommitted)
	// The rule binds only DELETE ledger; that candidate stays a bare IMPEACHMENT (from
	// does not bind the path) and blocks under require_proof, while audit_log — bound
	// by no rule — stays advisory. So exactly one blocker, the ledger one.
	if v := candidateFor(t, res.Report, "db DELETE ledger").Verdict; v != VerdictImpeachment {
		t.Fatalf("Verdict = %q, want bare %q (from does not bind the path)", v, VerdictImpeachment)
	}
	got := res.GateBlockers()
	if len(got) != 1 || got[0].Verdict != VerdictImpeachment || got[0].Rule != "routes-no-delete" {
		t.Fatalf("require_proof bare impeachment did not block: %+v", got)
	}
}
