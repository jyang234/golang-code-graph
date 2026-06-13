package fitness

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// checkMustPassThrough evaluates each waypoint invariant: every path from a
// From-matching function to a To-matching target (function or boundary effect)
// must pass through a Through-matching function. The check removes the waypoint
// nodes from the walk; any From→To path that remains is a bypass — a Violation
// naming the (source, target) pair, with one shortest bypass path as detail.
//
// Unlike must_not_reach's single witness, every unallowed bypass pair is
// reported: the base-vs-branch "new findings only" diff must surface a second
// bypass added on a branch even when the base already carries one.
//
// Three-valued like must_not_reach: no bypass over a blind frontier is a
// Caution ("cannot prove every path is guarded"), escalated by require_proof.
func checkMustPassThrough(p *policy.Policy, ix *graph.Index, r *Result) {
	for i := range p.MustPassThrough {
		rule := &p.MustPassThrough[i]
		throughLabel := shortPatterns(rule.Through)

		froms := bindFroms(ix, r, "must_pass_through", rule.Name, rule.From, rule.RequireProof)
		if froms == nil {
			continue
		}
		if !bindsAnyTarget(ix, rule.To) {
			r.add(unbindableTargetFinding("must_pass_through", rule.Name, "to", rule.RequireProof))
			continue
		}

		bypassed := false
		var blindEv evidence
		blind := false

		for _, from := range froms {
			if matchAny(from, rule.Through) {
				continue // the source IS the waypoint: trivially guarded
			}
			cone, parent := guardedWalk(ix, from, rule.Through)
			effects := ix.Effects(cone...)

			// A reachable function matching To, with the waypoints removed, is a
			// bypass. The source itself never matches as a target (a route that is
			// its own target is meaningless), but its direct effects count below.
			for _, fn := range cone {
				if fn != from && matchAny(fn, rule.To) && !rule.Allowed(from, fn) {
					bypassed = true
					r.add(Finding{
						Rule:     "must_pass_through",
						Severity: Violation,
						Summary:  fmt.Sprintf("%s: %s reaches %s without passing %s", rule.Name, ShortName(from), ShortName(fn), throughLabel),
						From:     from,
						To:       fn,
						Detail:   bypassPath(parent, from, fn, ""),
					})
				}
			}
			// A boundary effect made anywhere in the guarded-walk cone is reached
			// without a waypoint (waypoint nodes are never walked, so their own
			// effects never appear here).
			for _, e := range effects {
				if matchAny(e.To, rule.To) && !rule.Allowed(from, e.To) {
					bypassed = true
					r.add(Finding{
						Rule:     "must_pass_through",
						Severity: Violation,
						Summary:  fmt.Sprintf("%s: %s reaches %s without passing %s", rule.Name, ShortName(from), e.To, throughLabel),
						From:     from,
						To:       e.To,
						Detail:   bypassPath(parent, from, e.From, e.To),
					})
				}
			}
			// No bypass from this source: a blind node in the walked cone means
			// hidden edges could still skirt the waypoint — "guarded" is
			// unprovable. Probe only until the first blind site is found.
			if !blind {
				if site, isBlind := frontierBlindSiteWith(ix, cone, effects); isBlind {
					blind = true
					blindEv = evidence{from: from, target: site}
				}
			}
		}

		if !bypassed && blind {
			sev, note := Caution, "cannot prove every path is guarded"
			if rule.RequireProof {
				sev, note = Violation, "require_proof is set and guarding cannot be proven"
			}
			r.add(Finding{
				Rule:     "must_pass_through",
				Severity: sev,
				Summary:  fmt.Sprintf("%s: no bypass found, but the frontier is blind (%s) — %s", rule.Name, blindEv.target, note),
				From:     blindEv.from,
			})
		}
	}
}

// guardedWalk is a forward BFS from one source that never enters a
// Through-matching node, recording each node's BFS parent so a shortest bypass
// path can be rendered. The cone includes the source. Adjacency lists are
// pre-sorted, so the parent assignment — and therefore the witness path — is
// deterministic.
func guardedWalk(ix *graph.Index, from string, through []string) (cone []string, parent map[string]string) {
	parent = map[string]string{}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range ix.Callees(cur) {
			if seen[next] || matchAny(next, through) {
				continue
			}
			seen[next] = true
			parent[next] = cur
			queue = append(queue, next)
		}
	}
	cone = make([]string, 0, len(seen))
	for fqn := range seen {
		cone = append(cone, fqn)
	}
	sort.Strings(cone)
	return cone, parent
}

// bypassPath renders the shortest guarded-walk path from source to fn (plus a
// trailing boundary effect, when the target is one) — the witness the reviewer
// reads to see HOW the guard is skipped. Presentation only, never identity.
func bypassPath(parent map[string]string, from, fn, effect string) string {
	var rev []string
	for cur := fn; cur != from; cur = parent[cur] {
		rev = append(rev, ShortName(cur))
		if _, ok := parent[cur]; !ok {
			break
		}
	}
	parts := []string{ShortName(from)}
	for i := len(rev) - 1; i >= 0; i-- {
		parts = append(parts, rev[i])
	}
	if effect != "" {
		parts = append(parts, effect)
	}
	return strings.Join(parts, " → ")
}

// shortPatterns renders Through patterns compactly for summaries.
func shortPatterns(patterns []string) string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = ShortName(p)
	}
	return strings.Join(out, ", ")
}
