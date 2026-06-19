package fitness

import (
	"fmt"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// verdict is the three-valued outcome of a must-not-reach rule. The distinction
// between provenAbsent and noPathFound is the whole point: a static "no path"
// over a blind frontier (a reflect call, an unsafe package, a <dynamic> effect)
// is NOT a proof of safety, and presenting it as a clean pass would be the
// dangerous framing the design record warns against.
type verdict int

const (
	provenAbsent verdict = iota // no path, and the frontier is fully resolved — a real proof
	noPathFound                 // no path found, but the frontier is blind — cannot prove
	reachable                   // a path exists — the invariant is broken
)

// checkMustNotReach evaluates each negative reachability invariant: no function
// matching rule.From may transitively reach any target (function or boundary
// effect) matching rule.To. A reachable path is a Violation; an unprovable rule
// (no path, but a blind frontier) is a Caution naming where the graph went blind.
func checkMustNotReach(p *policy.Policy, ix *graph.Index, r *Result) {
	for _, rule := range p.MustNotReach {
		froms := bindFroms(ix, r, "must_not_reach", rule.Name, rule.From, rule.RequireProof)
		if froms == nil {
			continue
		}
		if !bindsAnyTarget(ix, rule.To) {
			r.add(unbindableTargetFinding("must_not_reach", rule.Name, "to", rule.RequireProof))
			continue
		}
		v, ev := evalReach(ix, froms, rule.To)
		switch v {
		case reachable:
			r.add(Finding{
				Rule:     "must_not_reach",
				Severity: Violation,
				Summary:  fmt.Sprintf("%s: %s reaches %s", rule.Name, ShortName(ev.from), ev.target),
				From:     ev.from,
				To:       ev.target,
			})
		case noPathFound:
			// Unprovable: no static path, but the frontier is blind. Advisory by
			// default; a require_proof rule treats unprovability as a failure.
			sev, note := Caution, "cannot prove absence"
			if rule.RequireProof {
				sev, note = Violation, "require_proof is set and absence cannot be proven"
			}
			r.add(Finding{
				Rule:     "must_not_reach",
				Severity: sev,
				Summary:  fmt.Sprintf("%s: no path found, but the frontier is blind (%s) — %s", rule.Name, ev.target, note),
				From:     ev.from,
			})
		case provenAbsent:
			// A real proof: nothing to report. Absence is the desired state.
		}
	}
}

// inertRuleFinding discloses a rule whose From binds nothing in this graph —
// the guard quietly stopped existing (a typo'd FQN, or a package renamed out
// from under the pattern), and the zero-seed walk would otherwise report it as
// a clean provenAbsent pass forever. A Caution by default; require_proof
// escalates to Violation — a rule that cannot even be evaluated is the
// strongest form of unprovability.
func inertRuleFinding(kind, name string, requireProof bool) Finding {
	sev, note := Caution, "inert rule"
	if requireProof {
		sev, note = Violation, "require_proof is set and an inert rule guards nothing"
	}
	return Finding{
		Rule:     kind,
		Severity: sev,
		Summary:  fmt.Sprintf("%s: from binds nothing in this graph — %s", name, note),
	}
}

// unbindableTargetFinding discloses a rule whose To/Through selector matches no
// node and no boundary effect ANYWHERE in the graph. The original design
// treated an empty To as the success state ("the forbidden thing does not
// exist"), but a real field run showed that is unsound: the graph cannot tell
// "the forbidden thing does not exist" from "the sink is unnameable" (a
// third-party logger whose methods are not graph nodes). A rule guarding an
// unnameable sink reports HOLDS forever while the unsafe call sits one line
// away — the exact silent pass the framework exists to prevent. So an
// unbindable target is disclosed like an inert From: a Caution by default,
// escalated to Violation under require_proof. (A To that DOES bind somewhere
// but is simply unreached stays a real proof — that is provenAbsent, untouched.)
func unbindableTargetFinding(kind, name, field string, requireProof bool) Finding {
	sev, note := Caution, "name a first-party sink it can bind, or this invariant is vacuous"
	if requireProof {
		sev, note = Violation, "require_proof is set and an unbindable target cannot be proven absent"
	}
	return Finding{
		Rule:     kind,
		Severity: sev,
		Summary:  fmt.Sprintf("%s: %s binds nothing in this graph — %s", name, field, note),
	}
}

// bindsAnyTarget reports whether any To/Through pattern matches at least one
// node or one boundary effect label in the whole graph — the test that
// separates an unbindable selector (a typo, a renamed-away or third-party
// target) from a well-formed one that is merely unreached.
func bindsAnyTarget(ix *graph.Index, patterns []string) bool {
	for _, fqn := range ix.Nodes() {
		if matchAny(fqn, patterns) {
			return true
		}
	}
	for _, e := range ix.Edges() {
		if e.Boundary != "" && matchAny(e.To, patterns) {
			return true
		}
	}
	return false
}

// evidence carries the witness for a verdict: for reachable, the from function
// and the matched target; for noPathFound, a from function and the blind site.
type evidence struct {
	from   string
	target string
}

// evalReach computes the rule verdict over all from-functions. The rule is
// reachable if any from reaches a To target; otherwise noPathFound if any from's
// reachable frontier is blind; otherwise provenAbsent. Reachable dominates
// noPathFound dominates provenAbsent, so the most consequential outcome wins.
func evalReach(ix *graph.Index, froms []string, toPatterns []string) (verdict, evidence) {
	var blindEv evidence
	blind := false
	for _, from := range froms {
		cone := append([]string{from}, ix.Reachable(from)...)
		effects := ix.Effects(cone...)

		// A reachable function matching a To pattern is a direct hit.
		for _, fn := range cone {
			if fn != from && matchAny(fn, toPatterns) {
				return reachable, evidence{from: from, target: fn}
			}
		}
		// A reachable boundary effect matching a To pattern is also a hit.
		for _, e := range effects {
			if matchAny(e.To, toPatterns) {
				return reachable, evidence{from: from, target: e.To}
			}
		}
		// No path from this seed: is the frontier sound enough to call it a
		// proof? Probe only until the first blind site is found.
		if !blind {
			if site, isBlind := frontierBlindSiteWith(ix, cone, effects); isBlind {
				blind = true
				blindEv = evidence{from: from, target: site}
			}
		}
	}
	if blind {
		return noPathFound, blindEv
	}
	return provenAbsent, evidence{}
}

// frontierBlindSiteWith reports whether any node in the reachable cone sits on a
// blind spot — a reflect/HighFanOut site, a function in an unsafe/cgo/linkname
// package, or a function that makes a <dynamic> boundary effect. If so, edges may
// be hidden past it and a "no path" conclusion is not sound. It returns a
// representative site for the caution message.
func frontierBlindSiteWith(ix *graph.Index, cone []string, effects []graph.Edge) (string, bool) {
	for _, fn := range cone {
		if bs, ok := firstReachBlinding(ix.BlindSpotsAt(fn)); ok {
			return fmt.Sprintf("%s at %s", bs.Kind, ShortName(fn)), true
		}
		if bs, ok := firstReachBlinding(ix.BlindSpotsAt(PkgOf(fn))); ok {
			return fmt.Sprintf("%s in %s", bs.Kind, PkgOf(fn)), true
		}
	}
	for _, e := range effects {
		if e.IsDynamic() {
			return "unresolved boundary effect " + e.To, true
		}
	}
	return "", false
}

// firstReachBlinding returns the first blind spot at a site that actually blinds
// reachability, skipping ExternalBoundaryCall. An external boundary call names a
// KNOWN third-party target whose body is the same out-of-module leaf the
// reachability index already stops at (graph.Index drops external edges), so it
// hides no in-scope first-party path: disclosing it must not turn the accepted
// external-leaf scope into a fresh abstention, or every PROVEN over a path that
// touches a vendored package would silently become CANT-PROVE. Every other kind
// (an UNKNOWN func-value target that could dispatch back into first-party code, a
// reflect call, an unsafe/cgo/linkname package) can hide an in-scope edge, so it
// stays blinding. This is the consumer half of ExternalBoundaryCall's
// disclosure-only contract (the producer comment in blindspots.Kind pins the other).
func firstReachBlinding(bs []graph.BlindSpot) (graph.BlindSpot, bool) {
	for _, b := range bs {
		if b.Kind == string(blindspots.ExternalBoundaryCall) {
			continue
		}
		return b, true
	}
	return graph.BlindSpot{}, false
}

// matchNodes returns the graph nodes whose FQN matches any pattern. The order is
// unspecified — expandFroms, its only caller, collects the result into a set and
// sorts it — so it ranges nodes unsorted to avoid a redundant per-call sort.
func matchNodes(ix *graph.Index, patterns []string) []string {
	var out []string
	ix.RangeNodes(func(fqn string) {
		if matchAny(fqn, patterns) {
			out = append(out, fqn)
		}
	})
	return out
}
