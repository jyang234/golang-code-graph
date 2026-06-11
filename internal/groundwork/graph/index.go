package graph

import (
	"sort"

	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Index is a queryable view of a Graph: node lookup, forward/reverse adjacency
// over the first-party function subgraph, the boundary-effect surface, and the
// blind-spot frontier. It is built once and is read-only thereafter, so it is
// safe to share across the surfaces that consume it.
//
// Adjacency is precomputed into sorted, de-duplicated callee/caller lists at
// construction, so the repeated reachability walks (one per source for the budget,
// one per from-set for must-not-reach, one per changed site for review reach)
// never re-sort a node's neighbours.
//
// Reachability is computed over function-to-function edges only. Boundary edges
// (DB/bus/outbound) are sinks: they are not traversed, but they are recorded so a
// caller can ask "what external effects does X reach" in one pass.
type Index struct {
	g       *Graph
	nodes   map[string]Node
	callees map[string][]string // sorted, deduped first-party callees, by caller FQN
	callers map[string][]string // sorted, deduped first-party callers, by callee FQN
	effOf   map[string][]Edge   // boundary edges, keyed by the first-party FQN that makes them
	blind   map[string][]BlindSpot
}

// NewIndex builds an Index over g. The graph is retained by reference; callers
// must not mutate it afterwards.
func NewIndex(g *Graph) *Index {
	ix := &Index{
		g:     g,
		nodes: make(map[string]Node, len(g.Nodes)),
		effOf: make(map[string][]Edge),
		blind: make(map[string][]BlindSpot),
	}
	for _, n := range g.Nodes {
		ix.nodes[n.FQN] = n
	}

	calleeSet := map[string]map[string]bool{}
	callerSet := map[string]map[string]bool{}
	for _, e := range g.Edges {
		switch {
		case e.IsBoundary():
			ix.effOf[e.From] = append(ix.effOf[e.From], e)
		case ix.isNode(e.To):
			addAdj(calleeSet, e.From, e.To)
			addAdj(callerSet, e.To, e.From)
		default:
			// An edge to a non-boundary target that is not a known node: a call
			// into something outside the emitted scope. flowmap does not normally
			// emit these in the full view, but tolerate them by ignoring for
			// reachability — they are neither a traversable function nor an effect.
		}
	}
	ix.callees = freezeAdj(calleeSet)
	ix.callers = freezeAdj(callerSet)

	for _, b := range g.BlindSpots {
		ix.blind[b.Site] = append(ix.blind[b.Site], b)
	}
	return ix
}

func addAdj(m map[string]map[string]bool, k, v string) {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	m[k][v] = true
}

func freezeAdj(m map[string]map[string]bool) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, vs := range m {
		out[k] = setutil.SortedKeys(vs)
	}
	return out
}

func (ix *Index) isNode(fqn string) bool { _, ok := ix.nodes[fqn]; return ok }

// Has reports whether fqn is a function node in the graph.
func (ix *Index) Has(fqn string) bool { return ix.isNode(fqn) }

// Node returns the node for fqn, if present.
func (ix *Index) Node(fqn string) (Node, bool) { n, ok := ix.nodes[fqn]; return n, ok }

// Nodes returns every function FQN, sorted.
func (ix *Index) Nodes() []string { return setutil.SortedKeys(ix.nodes) }

// Callees returns the direct first-party callees of fqn, sorted and de-duplicated.
// The returned slice is precomputed and must be treated as read-only.
func (ix *Index) Callees(fqn string) []string { return ix.callees[fqn] }

// Callers returns the direct first-party callers of fqn, sorted and de-duplicated.
// The returned slice is precomputed and must be treated as read-only.
func (ix *Index) Callers(fqn string) []string { return ix.callers[fqn] }

// Reachable returns every function transitively reachable from any seed by
// following call edges forward (the seeds' downstream / dependency cone),
// excluding the seeds themselves unless a seed is reachable from another. The
// result is sorted.
func (ix *Index) Reachable(seeds ...string) []string {
	return ix.walk(seeds, func(fqn string) []string { return ix.callees[fqn] })
}

// Reaching returns every function that can transitively reach any seed by
// following call edges backward (who breaks if a seed changes). The result is
// sorted, excluding the seeds themselves unless one reaches another.
func (ix *Index) Reaching(seeds ...string) []string {
	return ix.walk(seeds, func(fqn string) []string { return ix.callers[fqn] })
}

// walk is the shared transitive-closure BFS for Reachable/Reaching. Seeds are
// enqueued but not placed in the result set unless re-discovered through an edge,
// so a seed only appears if it is genuinely (re)reachable — e.g. via a cycle.
func (ix *Index) walk(seeds []string, next func(string) []string) []string {
	seen := make(map[string]bool)
	var queue []string
	enqueue := func(s string) {
		for _, t := range next(s) {
			if !seen[t] {
				seen[t] = true
				queue = append(queue, t)
			}
		}
	}
	for _, s := range seeds {
		enqueue(s)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		enqueue(cur)
	}
	return setutil.SortedKeys(seen)
}

// Sources returns the function nodes with no first-party caller — the structural
// entry points of the graph (mains, HTTP handlers and bus consumers resolved
// through dynamic dispatch, exported library functions). This is the graph-only
// approximation of "entrypoint"; flowmap's boundary contract can later refine
// these with their route/topic names. The result is sorted.
func (ix *Index) Sources() []string {
	out := make([]string, 0)
	for fqn := range ix.nodes {
		if len(ix.callers[fqn]) == 0 {
			out = append(out, fqn)
		}
	}
	sort.Strings(out)
	return out
}

// EntrypointCover returns the Sources from which fqn is transitively reachable —
// the entry points the function is live behind. The result is sorted.
func (ix *Index) EntrypointCover(fqn string) []string {
	reaching := setutil.StringSet(ix.Reaching(fqn))
	var out []string
	for _, s := range ix.Sources() {
		if s == fqn || reaching[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Effects returns the boundary effects (DB/bus/outbound edges) made directly by
// any function in fqns. Order follows the input; callers wanting the full
// downstream effect surface of a function pass Reachable(fqn) plus fqn.
func (ix *Index) Effects(fqns ...string) []Edge {
	var out []Edge
	for _, fqn := range fqns {
		out = append(out, ix.effOf[fqn]...)
	}
	return out
}

// BlindSpotsAt returns the blind spots recorded at a site (an FQN or a package
// path).
func (ix *Index) BlindSpotsAt(site string) []BlindSpot { return ix.blind[site] }

// BlindSpots returns the whole graph-completeness blind-spot manifest.
func (ix *Index) BlindSpots() []BlindSpot { return ix.g.BlindSpots }

// Edges returns every edge in the underlying graph (internal and boundary),
// for checks that need edge attributes the adjacency lists drop (concurrent).
func (ix *Index) Edges() []Edge { return ix.g.Edges }

// Obligations returns the graph's path-obligation verdicts.
func (ix *Index) Obligations() []Obligation { return ix.g.Obligations }

// EffectOrder returns the graph's partial-effect order facts.
func (ix *Index) EffectOrder() []EffectOrderFact { return ix.g.EffectOrder }
