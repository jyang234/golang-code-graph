package review

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// TestContractNonBreakingUnderHandlerRefactorOverGeneratedGraphs makes the R10
// invariant literal across INPUTS, not just the hand-authored fixture: for any
// service graph, a refactor that MOVES a route's handler symbol while leaving the
// route name intact must produce NO breaking entrypoint contract change. This is
// the review/contract analogue of the proposer's generated-graph self-clean
// property (fitness): R3/R7/R10 were each a case where a regression fixture's
// topology didn't match the field's, so the over-fire slipped through a green
// test. A generated corpus — inline-closure handlers renumbered by an
// extract-function refactor (run$4 → newHTTPServer$1), named handlers plainly
// renamed (H → HV2), and §9 internal-root churn mixed in — is what catches the
// next sibling before a field report does.
//
// The negative control on each seed (drop a real route) keeps the property from
// passing vacuously: a genuinely removed route name must still BLOCK as breaking.
// Seeds are fixed and logged, so any failure reproduces deterministically.
func TestContractNonBreakingUnderHandlerRefactorOverGeneratedGraphs(t *testing.T) {
	const seeds = 400
	p := &policy.Policy{Service: "svc", Version: 1}
	for s := int64(0); s < seeds; s++ {
		r := rand.New(rand.NewSource(s))
		base, refactored, routes, topic := genRouteGraphs(r)

		// The refactor: handler symbols AND the effect emitter moved, route names
		// and the published topic unchanged → both contract surfaces are intact,
		// so nothing here may read as a breaking or even an entrypoint change.
		a := Review(p, base, refactored)
		for _, c := range a.Contract {
			if c.Surface == "entrypoint" {
				t.Fatalf("seed %d: a handler refactor with stable route names produced an entrypoint contract change %+v\n%s",
					s, c, dumpRoutes(base, refactored))
			}
		}
		if anyBreaking(a.Contract) {
			t.Fatalf("seed %d: a handler/emitter refactor with stable contract names was judged breaking: %+v\n%s",
				s, a.Contract, dumpRoutes(base, refactored))
		}

		// Negative control: genuinely drop one route (its entrypoint leaves the
		// join) — that route name must still surface as a breaking removal.
		dropIdx := r.Intn(len(routes))
		dropped := routes[dropIdx]
		ctrl := dropRoute(base, dropped)
		b := Review(p, base, ctrl)
		var sawDropped bool
		for _, c := range b.Contract {
			if c.Op == "-" && c.Surface == "entrypoint" && c.Breaking && c.Name == dropped {
				sawDropped = true
			}
		}
		if !sawDropped {
			t.Fatalf("seed %d: dropping route %q must be a breaking entrypoint removal; got %+v",
				s, dropped, b.Contract)
		}

		// Effect-path control: genuinely drop the published topic (remove the
		// emitter edge) — the effect-set delta must still surface a breaking
		// removal, so the non-breaking property above is not vacuous for effects.
		c2 := Review(p, base, dropEffectTarget(base, topic))
		var sawEffectDrop bool
		for _, c := range c2.Contract {
			if c.Op == "-" && c.Surface == "publish" && c.Breaking {
				sawEffectDrop = true
			}
		}
		if !sawEffectDrop {
			t.Fatalf("seed %d: dropping the published topic must be a breaking effect removal; got %+v",
				s, c2.Contract)
		}
	}
}

// genRouteGraphs builds a base service graph and its refactored branch. Every
// route's handler is moved between base and branch — an inline-closure handler
// renumbered by an extract-function refactor (run$N → newHTTPServer$M), or a
// named handler plainly renamed (H{i} → H{i}V2) — while the route NAME and kind
// are held identical. Each handler delegates into a shared, unmoved store node so
// no effect-source movement clouds the entrypoint contract delta. It also carries
// one CONTRACT effect (a published topic) emitted by a function the refactor MOVES
// (Emit → EmitV2), with the topic held stable — so the effect-set contract path is
// exercised under emitter movement too (the R10-sibling fix), not just entrypoints.
// The published topic is returned so the caller can drop it as a negative control.
// Internal (non-entrypoint) root churn is mixed in so §9's orphan-root exclusion is
// exercised alongside R10. Deterministic in r.
func genRouteGraphs(r *rand.Rand) (base, refactored *graph.Graph, routes []string, topic string) {
	const mod = "example.com/svc/"
	const store = "(*" + mod + "internal/store.Store).Exec"
	storeEffect := graph.Edge{From: store, To: "boundary:db UPDATE users", Tier: 1, Boundary: "outbound-sync"}

	baseNodes := []graph.Node{{FQN: store, Sig: "func() error", Tier: 2}}
	brNodes := []graph.Node{{FQN: store, Sig: "func() error", Tier: 2}}
	baseEdges := []graph.Edge{storeEffect}
	brEdges := []graph.Edge{storeEffect}
	var baseEps, brEps []graph.Entrypoint

	methods := []string{"GET", "POST", "PUT", "DELETE"}
	k := 2 + r.Intn(5) // 2..6 routes
	for i := 0; i < k; i++ {
		kind, name := "http", fmt.Sprintf("%s /v1/r%d", methods[r.Intn(len(methods))], i)
		if r.Intn(100) < 25 {
			kind, name = "consumer", fmt.Sprintf("topic.r%d", i)
		}

		var bfn, rfn string
		if r.Intn(2) == 0 {
			// Inline-closure handler: a synthetic, position-derived FQN that an
			// extract-function refactor renumbers (the run$4 → newHTTPServer$1 case).
			const pkg = mod + "cmd/svc"
			bfn = fmt.Sprintf("%s.run$%d", pkg, 1+r.Intn(9))
			rfn = fmt.Sprintf("%s.newHTTPServer$%d", pkg, 1+r.Intn(9))
		} else {
			// Named handler plainly renamed (attribution #1: the over-fire is
			// general to any handler-symbol move, not just inline closures).
			bfn = fmt.Sprintf("(*%sinternal/handler.Server).H%d", mod, i)
			rfn = bfn + "V2"
		}

		baseNodes = append(baseNodes, graph.Node{FQN: bfn, Sig: "func() error", Tier: 1})
		brNodes = append(brNodes, graph.Node{FQN: rfn, Sig: "func() error", Tier: 1})
		baseEdges = append(baseEdges, graph.Edge{From: bfn, To: store, Tier: 2})
		brEdges = append(brEdges, graph.Edge{From: rfn, To: store, Tier: 2})
		baseEps = append(baseEps, graph.Entrypoint{Kind: kind, Name: name, Fn: bfn})
		brEps = append(brEps, graph.Entrypoint{Kind: kind, Name: name, Fn: rfn})
		routes = append(routes, name)
	}

	// A contract EFFECT (a published topic) emitted by a function the refactor
	// MOVES (Emit → EmitV2). The topic stands; only the emitter FQN changes, so
	// the effect-set contract path must read it as unchanged — exercising the
	// R10-sibling fix across the corpus.
	topic = fmt.Sprintf("boundary:bus PUBLISH topic.evt%d", r.Intn(1<<16))
	emitterBase := fmt.Sprintf("(*%sinternal/notify.Notifier).Emit", mod)
	emitterBranch := emitterBase + "V2"
	baseNodes = append(baseNodes, graph.Node{FQN: emitterBase, Sig: "func() error", Tier: 1})
	baseEdges = append(baseEdges, graph.Edge{From: emitterBase, To: topic, Tier: 1, Boundary: "outbound-sync"})
	brNodes = append(brNodes, graph.Node{FQN: emitterBranch, Sig: "func() error", Tier: 1})
	brEdges = append(brEdges, graph.Edge{From: emitterBranch, To: topic, Tier: 1, Boundary: "outbound-sync"})

	// §9 internal churn: a non-entrypoint root present only in base and a
	// different one only in branch — neither is in the entrypoints join, so
	// neither may read as a contract change.
	orphanBase := fmt.Sprintf("%sinternal/worker.poll%d", mod, r.Intn(1<<16))
	orphanBranch := fmt.Sprintf("%sinternal/worker.flush%d", mod, r.Intn(1<<16))
	baseNodes = append(baseNodes, graph.Node{FQN: orphanBase, Sig: "func()", Tier: 1})
	baseEdges = append(baseEdges, graph.Edge{From: orphanBase, To: store, Tier: 2})
	brNodes = append(brNodes, graph.Node{FQN: orphanBranch, Sig: "func()", Tier: 1})
	brEdges = append(brEdges, graph.Edge{From: orphanBranch, To: store, Tier: 2})

	base = &graph.Graph{Algo: "vta", Nodes: baseNodes, Edges: baseEdges, Entrypoints: baseEps}
	refactored = &graph.Graph{Algo: "vta", Nodes: brNodes, Edges: brEdges, Entrypoints: brEps}
	return base, refactored, routes, topic
}

// dropEffectTarget returns a copy of g with every edge to the given boundary
// target removed — a genuine effect-contract removal, the negative control for
// the effect-set non-breaking property.
func dropEffectTarget(g *graph.Graph, to string) *graph.Graph {
	edges := make([]graph.Edge, 0, len(g.Edges))
	for _, e := range g.Edges {
		if e.To == to {
			continue
		}
		edges = append(edges, e)
	}
	return &graph.Graph{Algo: g.Algo, Nodes: g.Nodes, Edges: edges, Entrypoints: g.Entrypoints}
}

// dropRoute returns a copy of g with the named route's entrypoint (and its
// handler node and delegating edge) removed — a genuine external-contract
// removal, the negative control for the non-breaking property.
func dropRoute(g *graph.Graph, name string) *graph.Graph {
	var droppedFn string
	eps := make([]graph.Entrypoint, 0, len(g.Entrypoints))
	for _, ep := range g.Entrypoints {
		if ep.Name == name {
			droppedFn = ep.Fn
			continue
		}
		eps = append(eps, ep)
	}
	nodes := make([]graph.Node, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.FQN == droppedFn {
			continue
		}
		nodes = append(nodes, n)
	}
	edges := make([]graph.Edge, 0, len(g.Edges))
	for _, e := range g.Edges {
		if e.From == droppedFn {
			continue
		}
		edges = append(edges, e)
	}
	return &graph.Graph{Algo: g.Algo, Nodes: nodes, Edges: edges, Entrypoints: eps}
}

// dumpRoutes renders the route-name → handler-FQN binding of both sides so a
// failing seed is debuggable from the log alone.
func dumpRoutes(base, branch *graph.Graph) string {
	render := func(label string, g *graph.Graph) string {
		lines := make([]string, 0, len(g.Entrypoints))
		for _, ep := range g.Entrypoints {
			lines = append(lines, fmt.Sprintf("  %-18s %s -> %s", ep.Kind, ep.Name, ep.Fn))
		}
		sort.Strings(lines)
		return label + ":\n" + strings.Join(lines, "\n")
	}
	return render("base", base) + "\n" + render("branch", branch)
}
