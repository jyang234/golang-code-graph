// Package graphio renders the static pipeline's NON-gated view: the full
// first-party call graph with signatures and typed boundary edges (static-
// extractor spec §2, §9). Unlike the boundary contract, this view DOES include DB
// edges and internal call structure — it is the richer "what can happen" map for
// human understanding and the AI-assist surface. It is regenerated on demand and
// never gated, because function-level structure churns under refactoring.
package graphio

import (
	"sort"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	cg "github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/signatures"
)

// Graph is the non-gated call-graph view, optionally scoped to one entry point.
// It carries the graph-completeness blind spots (reflect, high fan-out,
// unsafe/cgo/linkname) — disclosures that belong with the "what can happen" map
// rather than the gated boundary contract.
type Graph struct {
	Entrypoint string                 `json:"entrypoint,omitempty"`
	Nodes      []Node                 `json:"nodes"`
	Edges      []Edge                 `json:"edges"`
	BlindSpots []blindspots.BlindSpot `json:"blind_spots"`
}

// Node is one first-party function.
type Node struct {
	FQN      string `json:"fqn"`
	Sig      string `json:"sig"`
	Tier     int    `json:"tier"`
	Fallible bool   `json:"fallible,omitempty"`
}

// Edge is a call from a first-party function to another first-party function or
// to a typed boundary node (DB, external service, or bus).
type Edge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Tier       int    `json:"tier"`
	Boundary   string `json:"boundary,omitempty"`
	Concurrent bool   `json:"concurrent,omitempty"`
}

// Build renders the full first-party graph of res. If entry is non-empty, the
// graph is scoped to the functions reachable from the matching entry-point root.
func Build(res *analyze.Result, entry string) (*Graph, error) {
	ext := features.NewExtractor(res.Config, res.Program.ModulePath)
	hints := ext.Hints()

	scope := firstPartyScope(res)
	if entry != "" {
		root := rootByName(res, entry)
		if root == nil {
			return nil, &EntryNotFoundError{Entry: entry}
		}
		scope = reachableFirstParty(res, root)
	}

	g := &Graph{Entrypoint: entry, Nodes: []Node{}, Edges: []Edge{}, BlindSpots: []blindspots.BlindSpot{}}
	if gs := blindspots.Graph(blindspots.Detect(res, hints)); len(gs) > 0 {
		g.BlindSpots = gs
	}
	rootFns := rootFuncSet(res)

	for _, n := range res.Graph.Nodes {
		fn := n.Func
		if !scope[fn] {
			continue
		}
		g.Nodes = append(g.Nodes, Node{
			FQN:      fn.RelString(nil),
			Sig:      signatures.Of(fn),
			Tier:     nodeTier(ext, fn, rootFns[fn]),
			Fallible: fallible(fn),
		})
		for _, e := range n.Out {
			g.Edges = append(g.Edges, edgeOf(ext, hints, e, scope)...)
		}
	}

	sortGraph(g)
	return g, nil
}

// Marshal renders the graph as canonical JSON (non-gated, but still deterministic).
func (g *Graph) Marshal() ([]byte, error) { return canonjson.Marshal(g) }

// EntryNotFoundError reports that no entry-point root matched a --entry argument.
type EntryNotFoundError struct{ Entry string }

func (e *EntryNotFoundError) Error() string { return "no entry point named " + e.Entry }

// edgeOf renders zero or one graph edges for an SSA call edge: a typed boundary
// edge for publish/HTTP/DB calls, an internal edge for first-party→first-party
// calls, and nothing for calls into unhinted stdlib/third-party code.
func edgeOf(ext *features.Extractor, hints *features.HintSet, e *cg.Edge, scope map[*ssa.Function]bool) []Edge {
	from := e.Caller.Func.RelString(nil)
	callee := e.Callee.Func
	f := ext.Edge(e.Caller.Func, callee, e.Site)
	tier, _ := ext.Classify(f)
	concurrent := f.Concurrent

	switch {
	case hints.IsPublish(callee):
		return []Edge{{From: from, To: "boundary:bus PUBLISH " + eventLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
	case hints.IsHTTP(callee):
		return []Edge{{From: from, To: "boundary:" + httpLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
	case hints.IsDB(callee):
		return []Edge{{From: from, To: "boundary:db " + dbLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
	case scope[callee]:
		return []Edge{{From: from, To: callee.RelString(nil), Tier: tier, Concurrent: concurrent}}
	default:
		return nil // a call into unhinted stdlib/third-party code; not part of the view
	}
}

func nodeTier(ext *features.Extractor, fn *ssa.Function, isRoot bool) int {
	if isRoot {
		t, _ := ext.Classify(ext.Inbound(fn.RelString(nil), fallible(fn)))
		return t
	}
	t, _ := ext.Classify(ext.Edge(fn, fn, nil)) // self-edge: same-package, compute → first-party rule
	return t
}

func sortGraph(g *Graph) {
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].FQN < g.Nodes[j].FQN })
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Tier < b.Tier
	})
	g.Edges = dedupEdges(g.Edges)
}

func dedupEdges(in []Edge) []Edge {
	out := in[:0]
	var prev Edge
	for i, e := range in {
		if i > 0 && e == prev {
			continue
		}
		out = append(out, e)
		prev = e
	}
	return out
}
