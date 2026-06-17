package graph

import (
	"sort"
	"strings"

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
	sources []string // caller-less nodes, sorted; a pure function of the graph, computed once
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

	// Sources (caller-less nodes) are a pure function of the now-frozen adjacency,
	// so compute the sorted set once here instead of re-scanning every node and
	// re-sorting on every Sources() call (the ground card and every reach pass hit
	// this on the serving path).
	for fqn := range ix.nodes {
		if len(ix.callers[fqn]) == 0 {
			ix.sources = append(ix.sources, fqn)
		}
	}
	sort.Strings(ix.sources)
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

// RangeNodes calls f for every function FQN in arbitrary (map) order. Callers
// that filter into a set and re-sort the result themselves (matchNodes) use this
// to skip the full sort Nodes() performs on every call — it is called once per
// from-entry during proposal and enforcement.
func (ix *Index) RangeNodes(f func(fqn string)) {
	for fqn := range ix.nodes {
		f(fqn)
	}
}

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
	// Return a copy so a caller cannot mutate the cached, shared slice.
	return append([]string(nil), ix.sources...)
}

// EntrypointCover returns the Sources from which fqn is transitively reachable —
// the entry points the function is live behind. The result is sorted.
func (ix *Index) EntrypointCover(fqn string) []string {
	return ix.EntrypointCoverFrom(fqn, setutil.StringSet(ix.Reaching(fqn)))
}

// EntrypointCoverFrom is EntrypointCover for a caller that has ALREADY computed
// fqn's reverse-reachable set. The ground card needs both the cover and the raw
// reaching set, so this lets it pay for the (O(V+E)) reverse BFS once instead of
// twice on the serving path. reaching must be the set form of ix.Reaching(fqn).
func (ix *Index) EntrypointCoverFrom(fqn string, reaching map[string]bool) []string {
	var out []string
	for _, s := range ix.sources {
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

// Bus boundary-effect operations as flowmap labels them. Decoding these here
// keeps the label vocabulary in the package that owns the graph schema;
// consumers must never re-parse boundary labels themselves.
const (
	BusPublish = "PUBLISH"
	BusConsume = "CONSUME"
)

// BusEffect is one statically-named bus boundary effect: From publishes or
// consumes Event (Op is BusPublish or BusConsume).
type BusEffect struct {
	Op    string
	Event string
	From  string
}

// BusEffects decodes the graph's bus boundary surface: every statically-named
// publish/consume effect, plus the count of dynamically-named bus effects —
// the events this surface cannot name, which any consumer must disclose
// rather than ignore. Unrecognized label shapes are skipped, not guessed.
func (ix *Index) BusEffects() (effects []BusEffect, dynamic int) {
	prefix := boundaryPrefix + "bus "
	for _, e := range ix.g.Edges {
		if !strings.HasPrefix(e.To, prefix) {
			continue
		}
		if e.IsDynamic() {
			dynamic++
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(e.To, prefix))
		if len(fields) < 2 {
			continue
		}
		effects = append(effects, BusEffect{Op: fields[0], Event: fields[1], From: e.From})
	}
	return effects, dynamic
}

// DBEffect is one statically-named database boundary effect: From issues a SQL
// Op (DELETE, SELECT, …) against Table. The static label carries no DB *system*
// — SSA cannot see which database a `*sql.DB` points at — so a DBEffect names
// only what static can prove, and a consumer reconciling it against a behavioral
// "DB <system> <OP> <table>" op key must drop the system to compare (the
// behavioral-impeachment join, plan §14-A).
type DBEffect struct {
	Op    string // SQL operation verbatim from the label, e.g. DELETE
	Table string // target table/relation
	From  string // emitting first-party FQN
}

// DBEffects decodes the graph's database boundary surface: every statically-named
// db effect, plus the count of effects whose statement the labeler could not read
// (a dynamic/built statement that fell back to the driver method name, e.g.
// "boundary:db Exec" — one field, no table). Like BusEffects' dynamic count, the
// unreadable ones are TALLIED, not guessed: a consumer must disclose them rather
// than treat the surface as exhaustively named. Decoding here keeps the
// "boundary:db" label vocabulary with the schema owner (consumers never re-parse
// boundary labels themselves).
func (ix *Index) DBEffects() (effects []DBEffect, unreadable int) {
	prefix := boundaryPrefix + "db "
	for _, e := range ix.g.Edges {
		if !strings.HasPrefix(e.To, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(e.To, prefix))
		if len(fields) < 2 {
			// "<op>" alone (Exec/call/Scan) — opaque SQL the labeler could not
			// resolve to an op+table. Count it; never fabricate a table.
			unreadable++
			continue
		}
		effects = append(effects, DBEffect{Op: fields[0], Table: fields[1], From: e.From})
	}
	return effects, unreadable
}

// KindHighFanOut is the blind-spot kind flowmap records at a dynamic-dispatch
// site its algorithm resolved to many callees (an interface method, an
// oapi-codegen strictHandler seam). Decoding the vocabulary here keeps it with
// the schema owner: a backward reach that passes through such a site is
// OVER-approximated (the dispatch fans every caller onto every implementation),
// so an entrypoint cover counted across one is an upper bound, not a count.
const KindHighFanOut = "HighFanOut"

// CrossesHighFanOut reports whether any of the given nodes sits on a HighFanOut
// blind spot — the test a cover renderer uses to mark its count as an
// over-approximation when the reverse reach fanned out through a dispatch seam.
func (ix *Index) CrossesHighFanOut(nodes []string) bool {
	for _, fn := range nodes {
		for _, b := range ix.blind[fn] {
			if b.Kind == KindHighFanOut {
				return true
			}
		}
	}
	return false
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

// Frontier returns the producer's disclosed frontier section (markers, unconfirmed
// count, coverage), or nil when none was emitted. A read-only disclosure: no
// verdict reads it.
func (ix *Index) Frontier() *FrontierSection { return ix.g.Frontier }

// Algo returns the call-graph construction algorithm the graph was built on
// (rta|vta|cha), or "" when unrecorded — the substrate a verdict over this graph
// is computed on, and what a policy's recorded Substrate is checked against.
func (ix *Index) Algo() string { return ix.g.Algo }

// Entrypoints returns the graph's named roots (routes, topics) with handlers.
func (ix *Index) Entrypoints() []Entrypoint { return ix.g.Entrypoints }
