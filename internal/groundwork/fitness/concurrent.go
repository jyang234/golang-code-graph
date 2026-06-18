package fitness

import (
	"fmt"
	"sort"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// checkNoConcurrentReach evaluates each concurrency invariant: no target
// matching To may be reached along a path entered via a concurrent edge (a
// go/defer call site). Two ways in: a concurrent boundary edge IS the target
// directly (`go publish(...)`), or a concurrently-spawned function's forward
// cone reaches the target.
//
// The concurrent surface — spawned functions, their cone, the cone's effects,
// the blind probe — is rule-independent and computed once; each rule only
// filters it. Findings are emitted from a de-duplicated (from, target) pair
// set with one summary form, so the same target reached via several spawn
// sites (or via both a direct concurrent edge and the cone's effects) is one
// finding, not a multiset that churns the base-vs-branch diff.
//
// Three-valued like the other reach checks: no hit over a blind frontier is a
// Caution, escalated by require_proof.
func checkNoConcurrentReach(p *policy.Policy, ix *graph.Index, r *Result) {
	if len(p.NoConcurrentReach) == 0 {
		return
	}

	seedSet := map[string]bool{}
	var direct []graph.Edge
	for _, e := range ix.Edges() {
		if !e.Concurrent {
			continue
		}
		if e.IsBoundary() {
			direct = append(direct, e)
		} else if ix.Has(e.To) {
			seedSet[e.To] = true
		}
	}
	seeds := setutil.SortedKeys(seedSet)
	coneSet := setutil.StringSet(seeds)
	for _, fn := range ix.Reachable(seeds...) {
		coneSet[fn] = true
	}
	cone := setutil.SortedKeys(coneSet)
	effects := ix.Effects(cone...)
	blindSite, blindFound := frontierBlindSiteWith(ix, cone, effects)

	type pair struct{ from, to string }
	for _, rule := range p.NoConcurrentReach {
		// Parity with must_not_reach (reach.go): a To that binds nothing ANYWHERE in
		// the graph is a dead selector (a typo'd or stale label), not a proof the
		// concurrent cone is clean. Disclose it like an unbindable must_not_reach
		// target — a Caution by default, escalated under require_proof — so a guard
		// that quietly stopped existing is loud, not silently "enforced".
		if !bindsAnyTarget(ix, rule.To) {
			r.add(unbindableTargetFinding("no_concurrent_reach", rule.Name, "to", rule.RequireProof))
			continue
		}
		hits := map[pair]bool{}
		for _, e := range direct {
			if matchAny(e.To, rule.To) {
				hits[pair{e.From, e.To}] = true
			}
		}
		for _, fn := range cone {
			if matchAny(fn, rule.To) {
				hits[pair{"", fn}] = true
			}
		}
		for _, e := range effects {
			if matchAny(e.To, rule.To) {
				hits[pair{e.From, e.To}] = true
			}
		}

		keys := make([]pair, 0, len(hits))
		for k := range hits {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].from != keys[j].from {
				return keys[i].from < keys[j].from
			}
			return keys[i].to < keys[j].to
		})
		for _, k := range keys {
			r.add(Finding{
				Rule:     "no_concurrent_reach",
				Severity: Violation,
				Summary:  fmt.Sprintf("%s: %s reachable on a concurrent path", rule.Name, ShortName(k.to)),
				From:     k.from,
				To:       k.to,
			})
		}

		if len(hits) == 0 && blindFound {
			sev, note := Caution, "cannot prove the concurrent cone avoids the target"
			if rule.RequireProof {
				sev, note = Violation, "require_proof is set and avoidance cannot be proven"
			}
			r.add(Finding{
				Rule:     "no_concurrent_reach",
				Severity: sev,
				Summary:  fmt.Sprintf("%s: no concurrent path found, but the frontier is blind (%s) — %s", rule.Name, blindSite, note),
			})
		}
	}
}
