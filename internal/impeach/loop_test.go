package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/ir"
)

// prodL1 is the self-extinguish fixture: the shared l1Graph (a severed Purge that
// reaches the DELETE emitter) made impeachable — a code stamp plus production
// provenance — so the lone candidate is a true IMPEACHMENT localized to the precise
// node Site (*ex.com/svc.Admin).Purge, a node that DOES reach the emitter (so
// blinding it can extinguish the impeachment).
func prodL1(t *testing.T) (*graph.Index, Report, Provenance) {
	t.Helper()
	ix := graph.NewIndex(&graph.Graph{
		Stamp: "sha1",
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
	})
	prov := Provenance{TraceIdentity: "sha1", Capture: CaptureProduction}
	r := Audit("svc", ix, []*ir.CanonicalTrace{l1Trace()}, prov)
	if len(r.Candidates) != 1 || r.Candidates[0].Verdict != VerdictImpeachment {
		t.Fatalf("fixture is not a clean IMPEACHMENT: %+v", r.Candidates)
	}
	return ix, r, prov
}

// TestProposeRepairDefaultsToBlindSpot is the fail-toward-abstain gradient (§8):
// the loop proposes the ALWAYS-sound blind spot at the severance Site, never a
// reclaimer (which could fabricate reachability).
func TestProposeRepairDefaultsToBlindSpot(t *testing.T) {
	_, r, _ := prodL1(t)
	rep := ProposeRepair(r.Candidates[0])
	if rep == nil {
		t.Fatal("no repair proposed for an IMPEACHMENT")
	}
	if rep.Kind != RepairBlindSpot {
		t.Errorf("Kind = %q, want %q (never auto-propose a reclaimer)", rep.Kind, RepairBlindSpot)
	}
	if rep.Site != "(*ex.com/svc.Admin).Purge" {
		t.Errorf("Site = %q, want the severance Site", rep.Site)
	}
}

// TestProposeRepairNilForNonImpeachment: only IMPEACHMENT/VIOLATED carry a repair
// (§5). A downgrade proposes nothing — the loop never writes substrate off an
// unsound contradiction.
func TestProposeRepairNilForNonImpeachment(t *testing.T) {
	ix := l1Graph() // no stamp ⇒ VERSION-SKEW downgrade
	r := Audit("svc", ix, []*ir.CanonicalTrace{l1Trace()}, Provenance{})
	if r.Candidates[0].Verdict == VerdictImpeachment {
		t.Fatal("fixture should be a downgrade")
	}
	if rep := ProposeRepair(r.Candidates[0]); rep != nil {
		t.Errorf("repair proposed for a downgrade: %+v", rep)
	}
}

// TestProposeRepairNilForNoSeverance: a SeveranceNone witness (the §6 proof
// obligation failed — no real seam) has nothing to disclose without fabricating
// one, so no repair is proposed.
func TestProposeRepairNilForNoSeverance(t *testing.T) {
	w := Witness{Verdict: VerdictImpeachment, Severance: &Severance{Kind: SeveranceNone}}
	if rep := ProposeRepair(w); rep != nil {
		t.Errorf("repair proposed for SeveranceNone: %+v", rep)
	}
}

// TestRatchetEntryMatchesTheRepair is the §13-crack-#6 co-update: the ratchet
// allow-list entry a ratified repair commits is keyed (Kind, Site) so it allows
// EXACTLY the blind spot the repair adds — otherwise the disclosure would trip the
// blind_spot_ratchet, its sibling gate.
func TestRatchetEntryMatchesTheRepair(t *testing.T) {
	_, r, _ := prodL1(t)
	rep := ProposeRepair(r.Candidates[0])
	entry := RatchetEntry(rep, r.Candidates[0])
	ratchet := &policy.BlindSpotRatchet{Allow: []policy.BlindSpotException{entry}}
	if !ratchet.Allows(BlindSpotKindImpeachment, rep.Site) {
		t.Errorf("ratchet does not allow the repair it co-committed: kind=%q site=%q", BlindSpotKindImpeachment, rep.Site)
	}
	if entry.Reason == "" {
		t.Error("ratchet entry carries no reason (the witness justification, §8)")
	}
}

// TestRatifyBundlesBothHalves is the §8 enactment: a verified repair yields the
// ONE reviewed act — a declared seam for flowmap's config AND the policy ratchet
// allow-list entry — keyed identically (kind, site) and sharing the witness reason,
// so the two halves cannot drift and the ratchet allows exactly the declared seam.
func TestRatifyBundlesBothHalves(t *testing.T) {
	_, r, _ := prodL1(t)
	rep := ProposeRepair(r.Candidates[0])
	rat, ok := Ratify(rep, r.Candidates[0])
	if !ok {
		t.Fatal("Ratify refused a verified blind-spot repair")
	}
	if rat.DeclaredSite != rep.Site || rat.DeclaredKind != BlindSpotKindImpeachment {
		t.Errorf("declared seam = (%q,%q), want (%q,%q)", rat.DeclaredSite, rat.DeclaredKind, rep.Site, BlindSpotKindImpeachment)
	}
	// The two halves agree on (kind, site) and reason — one source, no drift.
	if rat.RatchetAllow.Site != rat.DeclaredSite || rat.RatchetAllow.Kind != rat.DeclaredKind || rat.RatchetAllow.Reason != rat.Reason {
		t.Errorf("ratchet entry %+v disagrees with the declared seam (%q,%q,%q)", rat.RatchetAllow, rat.DeclaredKind, rat.DeclaredSite, rat.Reason)
	}
	// The committed allow-list entry allows EXACTLY the declared seam (so enacting
	// it does not trip blind_spot_ratchet).
	ratchet := &policy.BlindSpotRatchet{Allow: []policy.BlindSpotException{rat.RatchetAllow}}
	if !ratchet.Allows(rat.DeclaredKind, rat.DeclaredSite) {
		t.Error("ratchet does not allow the seam it co-committed")
	}
	if rat.Reason == "" {
		t.Error("ratification carries no reason (the witness)")
	}
	// A reclaimer is never ratifiable through this path.
	if _, ok := Ratify(&ProposedRepair{Kind: RepairReclaimer, Site: "x"}, r.Candidates[0]); ok {
		t.Error("Ratify accepted a reclaimer")
	}
}

// TestSelfExtinguishesAcceptsCorrectRepair is the per-repair acceptance gate's
// happy path (§6/§8): blinding the precise severed Site makes the emitter
// seam-reachable, so the target impeachment extinguishes, and with no policy proofs
// to lose the monotonic criterion holds — the repair is accepted.
func TestSelfExtinguishesAcceptsCorrectRepair(t *testing.T) {
	ix, r, prov := prodL1(t)
	rep := ProposeRepair(r.Candidates[0])
	res := SelfExtinguishes(ix, rep, r.Candidates[0], []*ir.CanonicalTrace{l1Trace()}, prov, &policy.Policy{}, "svc")
	if !res.OK {
		t.Fatalf("correct repair rejected: %+v", res)
	}
	if !res.Extinguished || !res.Monotonic {
		t.Errorf("expected extinguished+monotonic: %+v", res)
	}
}

// TestSelfExtinguishesRejectsMislocalizedSite: a blind spot at a node that does NOT
// cover the witnessed seam (the entry handler, which reaches no emitter) leaves the
// target standing — the mislocalization announces itself, and the gate refuses the
// repair (§6 self-extinguish). You never have to trust the localizer.
func TestSelfExtinguishesRejectsMislocalizedSite(t *testing.T) {
	ix, r, prov := prodL1(t)
	bad := &ProposedRepair{Kind: RepairBlindSpot, Site: "ex.com/svc.handler"}
	res := SelfExtinguishes(ix, bad, r.Candidates[0], []*ir.CanonicalTrace{l1Trace()}, prov, &policy.Policy{}, "svc")
	if res.OK || res.Extinguished {
		t.Errorf("mislocalized repair accepted: %+v", res)
	}
}

// TestSelfExtinguishesIsMonotonicNotCountOne is the §13-crack-#4 correction: when
// one blind spot extinguishes SEVERAL impeachments at once (two severed effects
// behind the same node), the gate still ACCEPTS — the criterion is monotonic
// (target extinguishes, no proof created), never "the count drops by exactly one."
func TestSelfExtinguishesIsMonotonicNotCountOne(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Stamp: "sha1",
		Nodes: []graph.Node{
			{FQN: "ex.com/svc.handler"},
			{FQN: "(*ex.com/svc.Admin).Purge"},
			{FQN: "ex.com/svc.del"},
			{FQN: "ex.com/svc.ins"},
		},
		Edges: []graph.Edge{
			{From: "(*ex.com/svc.Admin).Purge", To: "ex.com/svc.del"},
			{From: "(*ex.com/svc.Admin).Purge", To: "ex.com/svc.ins"},
			{From: "ex.com/svc.del", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
			{From: "ex.com/svc.ins", To: "boundary:db INSERT audit", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "ex.com/svc.handler"}},
	})
	// One purge span (tagged) with TWO severed effect children — both impeached.
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{{
			Op: "internal purge", Kind: ir.KindInternal,
			Attrs: map[string]string{FQNTagKey: "ex.com/svc.(*Admin).Purge"},
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
				{Op: "DB postgres INSERT audit", Kind: ir.KindClient},
			}}},
		}}}},
	}}
	prov := Provenance{TraceIdentity: "sha1", Capture: CaptureProduction}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, prov)
	if len(r.Candidates) != 2 {
		t.Fatalf("want 2 impeachments, got %d", len(r.Candidates))
	}
	target := r.Candidates[0]
	rep := ProposeRepair(target)
	res := SelfExtinguishes(ix, rep, target, []*ir.CanonicalTrace{tr}, prov, &policy.Policy{}, "svc")
	if !res.OK {
		t.Errorf("repair extinguishing multiple impeachments was rejected: %+v", res)
	}
}

// TestSelfExtinguishesMonotonicWithProofs exercises the findingKeys path with a
// real policy: a must_not_reach Violation that holds BEFORE the repair must still
// hold AFTER (blinding adds disclosure but removes no edge, so a reachable path is
// untouched) — before ⊆ after, so the gate accepts.
func TestSelfExtinguishesMonotonicWithProofs(t *testing.T) {
	ix, r, prov := prodL1(t)
	rep := ProposeRepair(r.Candidates[0])
	p := &policy.Policy{MustNotReach: []policy.ReachRule{
		{Name: "purge-reaches-delete", From: []string{"(*ex.com/svc.Admin).Purge"}, To: []string{"boundary:db DELETE ledger"}},
	}}
	res := SelfExtinguishes(ix, rep, r.Candidates[0], []*ir.CanonicalTrace{l1Trace()}, prov, p, "svc")
	if !res.OK || !res.Monotonic {
		t.Errorf("monotonic repair rejected (a surviving Violation was misread as lost): %+v", res)
	}
}

// TestSelfExtinguishesRefusesReclaimer: a reclaimer is never auto-proposed and is
// not the blind-spot acceptance question, so the gate refuses to evaluate one
// (fail closed — it never silently accepts a reachability-fabricating repair).
func TestSelfExtinguishesRefusesReclaimer(t *testing.T) {
	ix, r, prov := prodL1(t)
	recl := &ProposedRepair{Kind: RepairReclaimer, Site: "(*ex.com/svc.Admin).Purge"}
	res := SelfExtinguishes(ix, recl, r.Candidates[0], []*ir.CanonicalTrace{l1Trace()}, prov, &policy.Policy{}, "svc")
	if res.OK {
		t.Errorf("gate accepted a reclaimer repair: %+v", res)
	}
}
