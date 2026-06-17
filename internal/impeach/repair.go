package impeach

import (
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// The discovery loop's proposal half (plan §8). The loop PROPOSES; a human
// ratifies — it never silently rewrites the soundness substrate (the same model as
// `init` proposing + a CODEOWNER committing). This file builds the proposal; the
// per-repair acceptance gate is extinguish.go.

// Repair kinds (§5/§8). The fail-toward-abstain gradient lives in which one the
// loop will auto-propose: only the always-sound blind_spot.
const (
	// RepairBlindSpot discloses the severance Site as a blind spot — ALWAYS sound:
	// it makes static abstain at the seam (NEVER→CANT-PROVE), so its worst failure
	// is over-abstention, never a fabricated reachability. The loop's default.
	RepairBlindSpot = "blind_spot"

	// RepairReclaimer adds a recovered edge — the PRECISE repair, but it fabricates
	// reachability, so a wrong edge mints a false VIOLATED. It needs human
	// ratification and is NEVER auto-proposed by the loop (§8); which severance bins
	// are reclaimer-safe is deferred to the reclaimer framework (§12.4).
	RepairReclaimer = "reclaimer"
)

// BlindSpotKindImpeachment is the graph.BlindSpot.Kind a ratified impeachment
// repair carries — a disclosed seam discovered by behavior, distinct from the
// statically-detected kinds (reflect, HighFanOut, …). It is also the ratchet
// allow-list entry's Kind, so the co-update (§8) matches the repair exactly.
const BlindSpotKindImpeachment = "ImpeachmentSeam"

// ProposedRepair mirrors blindspots.BlindSpot / frontier.Marker on purpose (§5):
// ratification is "move this proposed blind spot into the graph," not a
// translation (one source of truth for the blind-spot shape). It is a PROPOSAL —
// never enacted here.
type ProposedRepair struct {
	Kind   string `json:"kind"`             // RepairBlindSpot (default) | RepairReclaimer
	Site   string `json:"site"`             // the severance point (§6)
	Detail string `json:"detail,omitempty"` // why it is proposed (the witness)
}

// ProposeRepair builds the Phase-4 proposed substrate change for an impeached
// witness (§8): a DISCLOSED BLIND SPOT at the severance Site, always. The
// fail-toward-abstain gradient (§8): disclosing a blind spot is always safe
// (abstain), whereas a reclaimer is the precise repair that demands a provably-real
// edge — so the loop prefers disclosure and NEVER auto-proposes a reclaimer.
//
// Returns nil when there is no sound Site to repair: a witness that did not reach
// an impeaching verdict (only IMPEACHMENT/VIOLATED carry a repair, §5), or whose
// severance localized to no seam (SeveranceNone — the §6 proof obligation failed,
// so there is nothing to disclose without fabricating one).
func ProposeRepair(w Witness) *ProposedRepair {
	if w.Verdict != VerdictImpeachment && w.Verdict != VerdictViolated {
		return nil
	}
	if w.Severance == nil || w.Severance.Site == "" || w.Severance.Kind == SeveranceNone {
		return nil
	}
	return &ProposedRepair{
		Kind:   RepairBlindSpot,
		Site:   w.Severance.Site,
		Detail: "behavioral impeachment of " + w.Effect + " witnessed on flow " + w.Observed.Flow,
	}
}

// RatchetEntry is the blind-spot-ratchet allow-list entry a ratified repair
// co-commits in the SAME reviewed act (§8, §13 crack #6). Adding a blind spot is
// exactly what blind_spot_ratchet gates as "a new blind spot," so a ratification
// that disclosed the seam WITHOUT allow-listing it would trip that sibling gate.
// Keyed (Kind, Site) identically to the blind spot the repair adds, with the
// impeachment witness as its Reason — a reviewed, intentional disclosure, not drift.
func RatchetEntry(repair *ProposedRepair, w Witness) policy.BlindSpotException {
	return policy.BlindSpotException{
		Kind:   BlindSpotKindImpeachment,
		Site:   repair.Site,
		Reason: ratifyReason(w),
	}
}

// Ratification is the COMPLETE "one reviewed act" a human commits to enact a
// verified repair (§8, §13 crack #6) — both halves, derived from the same witness
// so they cannot drift:
//   - the ENACTMENT: a declared seam for flowmap's config (static.declaredBlindSpots),
//     so the next graph build carries the blind spot and static abstains at the seam;
//   - the RATCHET allow-list entry for the policy, so adding that blind spot does not
//     trip blind_spot_ratchet (its sibling gate).
//
// The declared-seam fields are plain strings (not a config.DeclaredBlindSpot) so the
// groundwork-side loop never imports the flowmap config package — the wire is the
// (kind, site, reason) triple, identical to the RatchetAllow entry's key.
type Ratification struct {
	// DeclaredSite/DeclaredKind/Reason render the flowmap config seam:
	//   static: {declaredBlindSpots: [{site: DeclaredSite, kind: DeclaredKind, reason: Reason}]}
	DeclaredSite string
	DeclaredKind string
	Reason       string
	// RatchetAllow is the policy blind_spot_ratchet allow-list entry, keyed
	// identically (kind, site) so it allows EXACTLY the declared seam.
	RatchetAllow policy.BlindSpotException
}

// Ratify bundles a verified blind-spot repair into the one reviewed act that
// enacts it (§8). It is pure derivation — it enacts nothing; the human commits the
// two halves (the flowmap config seam and the policy ratchet entry). Returns
// (_, false) when there is no blind-spot repair to ratify (a reclaimer is never
// auto-proposed, and a nil/empty repair has nothing to enact).
func Ratify(repair *ProposedRepair, w Witness) (Ratification, bool) {
	if repair == nil || repair.Kind != RepairBlindSpot || repair.Site == "" {
		return Ratification{}, false
	}
	return Ratification{
		DeclaredSite: repair.Site,
		DeclaredKind: BlindSpotKindImpeachment,
		Reason:       ratifyReason(w),
		RatchetAllow: RatchetEntry(repair, w),
	}, true
}

// ratifyReason is the ONE wording for a ratified seam's justification — the
// impeachment witness — shared by both halves of the ratification so the config
// seam and the ratchet entry record the same reason.
func ratifyReason(w Witness) string {
	return "ratified behavioral impeachment: " + w.Effect + " observed on flow " + w.Observed.Flow
}
