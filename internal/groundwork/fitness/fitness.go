package fitness

import (
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// Check evaluates every configured invariant in p against the graph ix and
// returns the findings, sorted deterministically (violations first). The result
// is a pure function of (policy, graph): the same inputs always produce the same
// findings in the same order, which is what lets a green verdict be a hard,
// reproducible CI gate.
func Check(p *policy.Policy, ix *graph.Index) Result {
	var r Result
	checkLayering(p, ix, &r)
	checkMustNotReach(p, ix, &r)
	checkMustPassThrough(p, ix, &r)
	checkNoConcurrentReach(p, ix, &r)
	checkIOBudget(p, ix, &r)
	checkObligations(p, ix, &r)
	checkRatchetCoupling(p, &r)
	r.sort()
	return r
}
