package fitness

import (
	"fmt"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
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
		froms := expandFroms(ix, rule.From)
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
		if bs := ix.BlindSpotsAt(fn); len(bs) > 0 {
			return fmt.Sprintf("%s at %s", bs[0].Kind, ShortName(fn)), true
		}
		if bs := ix.BlindSpotsAt(PkgOf(fn)); len(bs) > 0 {
			return fmt.Sprintf("%s in %s", bs[0].Kind, PkgOf(fn)), true
		}
	}
	for _, e := range effects {
		if e.IsDynamic() {
			return "unresolved boundary effect " + e.To, true
		}
	}
	return "", false
}

// matchNodes returns the graph nodes whose FQN matches any pattern.
func matchNodes(ix *graph.Index, patterns []string) []string {
	var out []string
	for _, fqn := range ix.Nodes() {
		if matchAny(fqn, patterns) {
			out = append(out, fqn)
		}
	}
	return out
}
