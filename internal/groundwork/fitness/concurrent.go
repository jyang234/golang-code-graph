package fitness

import (
	"fmt"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// checkNoConcurrentReach evaluates each concurrency invariant: no target
// matching To may be reached along a path entered via a concurrent edge (a
// go/defer call site). Two ways in: a concurrent boundary edge IS the target
// directly (`go publish(...)`), or a concurrent internal edge spawns a
// function whose forward cone reaches the target. Three-valued like the other
// reach checks: no hit over a blind frontier is a Caution, escalated by
// require_proof.
func checkNoConcurrentReach(p *policy.Policy, ix *graph.Index, r *Result) {
	for _, rule := range p.NoConcurrentReach {
		var seeds []string      // concurrently-spawned first-party functions
		var direct []graph.Edge // concurrent boundary effects
		for _, e := range ix.Edges() {
			if !e.Concurrent {
				continue
			}
			if e.IsBoundary() {
				direct = append(direct, e)
			} else if ix.Has(e.To) {
				seeds = append(seeds, e.To)
			}
		}

		hit := false
		for _, e := range direct {
			if matchAny(e.To, rule.To) {
				hit = true
				r.add(Finding{
					Rule:     "no_concurrent_reach",
					Severity: Violation,
					Summary:  fmt.Sprintf("%s: %s is made on a concurrent path at %s", rule.Name, e.To, ShortName(e.From)),
					From:     e.From,
					To:       e.To,
				})
			}
		}

		cone := append(append([]string{}, seeds...), ix.Reachable(seeds...)...)
		for _, fn := range cone {
			if matchAny(fn, rule.To) {
				hit = true
				r.add(Finding{
					Rule:     "no_concurrent_reach",
					Severity: Violation,
					Summary:  fmt.Sprintf("%s: %s reachable on a concurrent path", rule.Name, ShortName(fn)),
					To:       fn,
				})
			}
		}
		for _, e := range ix.Effects(cone...) {
			if matchAny(e.To, rule.To) {
				hit = true
				r.add(Finding{
					Rule:     "no_concurrent_reach",
					Severity: Violation,
					Summary:  fmt.Sprintf("%s: %s reachable on a concurrent path via %s", rule.Name, e.To, ShortName(e.From)),
					From:     e.From,
					To:       e.To,
				})
			}
		}

		if !hit {
			if site, isBlind := frontierBlindSite(ix, cone); isBlind {
				sev, note := Caution, "cannot prove the concurrent cone avoids the target"
				if rule.RequireProof {
					sev, note = Violation, "require_proof is set and avoidance cannot be proven"
				}
				r.add(Finding{
					Rule:     "no_concurrent_reach",
					Severity: sev,
					Summary:  fmt.Sprintf("%s: no concurrent path found, but the frontier is blind (%s) — %s", rule.Name, site, note),
				})
			}
		}
	}
}
