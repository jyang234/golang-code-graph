// Package graphio renders the static pipeline's NON-gated view: the full
// first-party call graph with signatures and typed boundary edges (static-
// extractor spec §2, §9). Unlike the boundary contract, this view DOES include DB
// edges and internal call structure — it is the richer "what can happen" map for
// human understanding and the AI-assist surface. It is regenerated on demand and
// never gated, because function-level structure churns under refactoring.
package graphio

import (
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	cg "github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/obligations"
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

	// Obligations is the path-obligation disclosure section: per-site verdicts
	// for the .flowmap.yaml obligation rules, FQN-keyed and separate from the
	// call-graph edges (a narrow level-2 slice). Omitted entirely when no rules
	// are configured, so rule-free services emit byte-identical graphs.
	Obligations []obligations.Finding `json:"obligations,omitempty"`

	// EffectOrder is the partial-effect disclosure (incident-triage plan IT-3):
	// for each function holding both a committed external effect (bus publish,
	// DB mutation) and a fallible call, whether the effect can — or always
	// does — execute before that call. Triage reads it to answer "if this call
	// faults, what may already be committed?". Like Obligations it rides
	// unscoped builds only and is omitted when empty.
	EffectOrder []obligations.EffectOrder `json:"effect_order,omitempty"`
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
	base := ""
	if entry == "" {
		abs, err := filepath.Abs(res.Dir)
		if err != nil {
			return nil, err
		}
		base = abs
	}

	for _, n := range res.Graph.Nodes {
		fn := n.Func
		if !scope[fn] {
			continue
		}
		// The node's outgoing edges are computed first because they decide the
		// node's tier: a function is as salient as the most consequential boundary
		// it directly reaches. Committed-effect sites (publish, DB mutation) are
		// collected in the same pass — this is the one place where the boundary
		// label and the ssa call site coexist (IT-3 scoping note).
		var nodeEdges []Edge
		var effectSites []obligations.EffectSite
		for _, e := range n.Out {
			edges := edgeOf(ext, hints, e, scope)
			nodeEdges = append(nodeEdges, edges...)
			if entry == "" && e.Site != nil && len(edges) == 1 && committedEffect(edges[0].To) {
				effectSites = append(effectSites, obligations.EffectSite{Label: edges[0].To, Site: e.Site})
			}
		}
		if entry == "" {
			g.EffectOrder = append(g.EffectOrder, obligations.OrderFacts(fn, effectSites, base)...)
		}
		g.Nodes = append(g.Nodes, Node{
			FQN:      fn.RelString(nil),
			Sig:      signatures.Of(fn),
			Tier:     nodeTier(ext, fn, rootFns[fn], nodeEdges),
			Fallible: fallible(fn),
		})
		g.Edges = append(g.Edges, nodeEdges...)
	}

	// Obligations are a whole-service disclosure (a level-2 slice of the FULL
	// graph). An entry-scoped view evaluates only the entry's cone, where a
	// rule anchored elsewhere would read UNMATCHED ("inert") and out-of-cone
	// verdicts would vanish — scoping artifacts presented as rule deadness. So
	// the section rides unscoped builds only.
	if rules := res.Config.Obligations; len(rules) > 0 && entry == "" {
		var fns []*ssa.Function
		for _, n := range res.Graph.Nodes {
			if scope[n.Func] {
				fns = append(fns, n.Func)
			}
		}
		g.Obligations = obligations.Check(rules, fns, base)
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
	case hints.IsConsume(callee):
		return []Edge{{From: from, To: "boundary:bus CONSUME " + eventLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
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

// committedEffect reports whether a boundary label is a committed external
// effect for partial-effect purposes: a bus publish or a DB mutation. Reads
// and outbound queries are not "committed" — re-running them is safe.
func committedEffect(label string) bool {
	if strings.HasPrefix(label, "boundary:bus PUBLISH") {
		return true
	}
	if strings.HasPrefix(label, "boundary:db ") {
		op := strings.Fields(strings.TrimPrefix(label, "boundary:db "))
		return len(op) > 0 && (op[0] == "INSERT" || op[0] == "UPDATE" || op[0] == "DELETE")
	}
	return false
}

// nodeTier ranks a function by what it does, not by what it is. A root is its
// inbound entry tier. Every other function takes the min over its direct
// outgoing edge tiers, falling back to the function's own compute floor (its
// internal same-package self-edge, tier 3 by default) when it reaches no
// consequential boundary. This is direct, not transitive — a helper that
// performs a DB read surfaces as tier 2 and one that publishes as tier 1, while a
// function that merely calls such helpers does not inherit their tier (so
// salience does not propagate up from main). Without this, classifying a function
// by its self-edge alone left every non-root function stuck at the compute floor.
func nodeTier(ext *features.Extractor, fn *ssa.Function, isRoot bool, outEdges []Edge) int {
	if isRoot {
		t, _ := ext.Classify(ext.Inbound(fn.RelString(nil), fallible(fn)))
		return t
	}
	// The self-edge (fn→fn) is the function's compute floor: internal,
	// same-package, no effect — tier 3 under the default rules.
	tier, _ := ext.Classify(ext.Edge(fn, fn, nil))
	for _, e := range outEdges {
		if e.Tier < tier {
			tier = e.Tier
		}
	}
	return tier
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
