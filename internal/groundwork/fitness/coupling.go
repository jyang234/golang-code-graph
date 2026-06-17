package fitness

import "github.com/jyang234/golang-code-graph/internal/groundwork/policy"

// checkRatchetCoupling surfaces the effect/blind-spot ratchet coupling caution
// (policy.EffectRatchetCouplingCaution — the single source shared with policy-check)
// as a graph-independent Caution. It takes no graph because the property is purely
// the policy's config: gating the effect ratchet without gating blind_spot_ratchet
// leaves the dynamic-laundering escape open (a new write hidden behind dynamic
// dispatch collapses to an existing <dynamic> label and evades the effect ratchet's
// label diff, with its only backstop ungated).
//
// It is a Caution, never a Violation: a disclosure of a soundness gap in the
// CONFIG, not a broken invariant in the code, so it must never flip the gate. Being
// policy-only it holds identically on base and branch, so review/verify surface it
// as a STANDING caution (R1) — disclosed at the gate where the "gate effect first"
// decision is actually made, not only in the fitness lens.
//
// The Rule is "ratchet_coupling", NOT "effect_ratchet": this is a config-coupling
// disclosure, distinct from the effect ratchet's write-target drift findings (which
// live in review, keyed under the effect-ratchet surface). A separate rule name
// keeps the two from reading as the same kind of finding.
func checkRatchetCoupling(p *policy.Policy, r *Result) {
	if c := p.EffectRatchetCouplingCaution(); c != "" {
		r.add(Finding{Rule: "ratchet_coupling", Severity: Caution, Summary: c})
	}
}
