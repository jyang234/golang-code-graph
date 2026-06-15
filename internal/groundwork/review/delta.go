package review

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// graphDelta is the set-based difference between two graphs: which nodes, internal
// edges, and boundary effects the branch added or removed. It is deliberately
// set-based (the known limitation from the pressure test: a new call *site* to an
// already-called target produces no "added edge"); the structural claims the
// artifact makes are scoped to exactly this.
type graphDelta struct {
	nodesAdded     []string
	nodesRemoved   []string
	edgesAdded     [][2]string
	edgesRemoved   [][2]string
	effectsAdded   []graph.Edge
	effectsRemoved []graph.Edge
}

// diffGraphs computes the base→branch delta.
func diffGraphs(base, branch *graph.Graph) graphDelta {
	var d graphDelta
	d.nodesAdded, d.nodesRemoved = diffStringSets(nodeSet(branch), nodeSet(base))

	baseInt, baseEff := edgeSets(base)
	branchInt, branchEff := edgeSets(branch)
	for k := range branchInt {
		if !baseInt[k] {
			d.edgesAdded = append(d.edgesAdded, splitEdge(k))
		}
	}
	for k := range baseInt {
		if !branchInt[k] {
			d.edgesRemoved = append(d.edgesRemoved, splitEdge(k))
		}
	}
	for k, e := range branchEff {
		if _, ok := baseEff[k]; !ok {
			d.effectsAdded = append(d.effectsAdded, e)
		}
	}
	for k, e := range baseEff {
		if _, ok := branchEff[k]; !ok {
			d.effectsRemoved = append(d.effectsRemoved, e)
		}
	}
	d.sort()
	return d
}

func (d *graphDelta) sort() {
	sort.Strings(d.nodesAdded)
	sort.Strings(d.nodesRemoved)
	sortEdges(d.edgesAdded)
	sortEdges(d.edgesRemoved)
	sortEffects(d.effectsAdded)
	sortEffects(d.effectsRemoved)
}

func (d graphDelta) empty() bool {
	return len(d.nodesAdded)+len(d.nodesRemoved)+
		len(d.edgesAdded)+len(d.edgesRemoved)+
		len(d.effectsAdded)+len(d.effectsRemoved) == 0
}

// touchedPackages is every package an added/removed node, internal edge endpoint,
// or effect source belongs to — the basis for the shape label.
func (d graphDelta) touchedPackages() []string {
	set := map[string]bool{}
	mark := func(fqn string) {
		if p := fitness.PkgOf(fqn); p != "" {
			set[p] = true
		}
	}
	for _, n := range d.nodesAdded {
		mark(n)
	}
	for _, n := range d.nodesRemoved {
		mark(n)
	}
	for _, e := range append(append([][2]string{}, d.edgesAdded...), d.edgesRemoved...) {
		mark(e[0])
		mark(e[1])
	}
	for _, e := range append(append([]graph.Edge{}, d.effectsAdded...), d.effectsRemoved...) {
		mark(e.From)
	}
	return setutil.SortedKeys(set)
}

// shape classifies the reach of the change by how many packages it touches.
func (d graphDelta) shape() Shape {
	if d.empty() {
		return BodyOnly
	}
	switch n := len(d.touchedPackages()); {
	case n <= 1:
		return Localized
	case n <= 3:
		return CrossPackage
	default:
		return Broad
	}
}

// pkgDeltas is the per-package node add/remove tally, for the "Touches" line.
func (d graphDelta) pkgDeltas() []PkgDelta {
	m := map[string]*PkgDelta{}
	get := func(pkg string) *PkgDelta {
		if m[pkg] == nil {
			m[pkg] = &PkgDelta{Package: pkg}
		}
		return m[pkg]
	}
	// Guard against an unparseable FQN (PkgOf == "") so the Touches line never
	// shows a nameless "(+1)", matching touchedPackages's filtering.
	for _, n := range d.nodesAdded {
		if pkg := fitness.PkgOf(n); pkg != "" {
			get(pkg).NodesAdded++
		}
	}
	for _, n := range d.nodesRemoved {
		if pkg := fitness.PkgOf(n); pkg != "" {
			get(pkg).NodesRemoved++
		}
	}
	out := make([]PkgDelta, 0, len(m))
	for _, pd := range m {
		out = append(out, *pd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	return out
}

// --- set helpers -----------------------------------------------------------

// edgeSet collects, into a set, the label extract returns for each edge where it
// reports ok. It is the iterate-filter-dedupe skeleton shared by the contract
// effect surface (contractEffects, keyed on the effect target) and the write
// ratchet (newWriteTargets, keyed on the write label) — the two differ only in
// the per-edge extractor.
func edgeSet[T comparable](edges []graph.Edge, extract func(graph.Edge) (T, bool)) map[T]bool {
	out := map[T]bool{}
	for _, e := range edges {
		if v, ok := extract(e); ok {
			out[v] = true
		}
	}
	return out
}

func nodeSet(g *graph.Graph) map[string]bool {
	s := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		s[n.FQN] = true
	}
	return s
}

// edgeSets splits a graph's edges into the internal function→function set (keyed
// "from\x00to") and the boundary-effect map (same key → the Edge, kept for
// display).
func edgeSets(g *graph.Graph) (map[string]bool, map[string]graph.Edge) {
	internal := map[string]bool{}
	effects := map[string]graph.Edge{}
	for _, e := range g.Edges {
		k := e.From + "\x00" + e.To
		if e.IsBoundary() {
			effects[k] = e
		} else {
			internal[k] = true
		}
	}
	return internal, effects
}

func splitEdge(key string) [2]string {
	from, to, _ := strings.Cut(key, "\x00")
	return [2]string{from, to}
}

func diffStringSets(a, b map[string]bool) (onlyA, onlyB []string) {
	for k := range a {
		if !b[k] {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if !a[k] {
			onlyB = append(onlyB, k)
		}
	}
	return onlyA, onlyB
}

func sortEdges(es [][2]string) {
	sort.Slice(es, func(i, j int) bool {
		if es[i][0] != es[j][0] {
			return es[i][0] < es[j][0]
		}
		return es[i][1] < es[j][1]
	})
}

func sortEffects(es []graph.Edge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].From != es[j].From {
			return es[i].From < es[j].From
		}
		return es[i].To < es[j].To
	})
}
