package fitness

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// TestProposeSelfCleanOverGeneratedGraphs makes the proposer/enforcer self-clean
// invariant literal across INPUTS, not just the hand-authored fixtures: for any
// graph, `init`'s proposed policy must pass `fitness` on that same graph with no
// proposer-derived violation. R5–R8 were each a node-set gap between a proposer
// and the enforcer that a fixture happened not to cover; a generated corpus is
// what catches the next sibling before a field report does. The invariant rests
// on reconcile relaxing/withdrawing anything the graph already violates, so a
// failure here is a real proposer/enforcer disagreement (or a reconcile
// non-convergence), not noise. Seeds are fixed and logged, so any failure
// reproduces deterministically and prints the offending graph.
func TestProposeSelfCleanOverGeneratedGraphs(t *testing.T) {
	const seeds = 400
	for s := int64(0); s < seeds; s++ {
		g := genGraph(rand.New(rand.NewSource(s)))
		ix := graph.NewIndex(g)
		p, _ := Propose(ix, "svc")
		if err := p.Validate(); err != nil {
			t.Fatalf("seed %d: init produced an invalid policy: %v\n%s", s, err, dumpGraph(g))
		}
		res := Check(p, ix)
		for _, f := range res.Violations() {
			// Obligation verdicts are graph-carried, not proposer-derived (init
			// cannot excuse them); the generator emits none, but skip defensively.
			if f.Rule == "obligation" {
				continue
			}
			t.Fatalf("seed %d: init's output violates its own graph — proposer/enforcer disagree: %+v\n%s", s, f, dumpGraph(g))
		}

		// The effect-ratchet analog of the self-clean invariant: the proposed
		// baseline must cover every current write target, so newWriteTargets(p,g,g)
		// is empty — init never reports its own write surface as new. This needs its
		// OWN check because reconcile re-runs Check (fitness), while the effect
		// ratchet is a review/Gate diff invisible to Check; a proposer that missed a
		// label would slip past the violations loop above (R12).
		for _, e := range ix.Edges() {
			label, ok := WriteLabel(e)
			if !ok {
				continue
			}
			if p.EffectRatchet == nil || !p.EffectRatchet.Allows(label) {
				t.Fatalf("seed %d: write target %q is not in the effect_ratchet baseline — it would fire as new on init's own graph\n%s", s, label, dumpGraph(g))
			}
		}
	}
}

// genGraph builds one pseudo-random service-shaped graph from r: nodes across a
// few layered packages, a random forward edge structure (roots, fan-out, the
// occasional cycle), an optional oapi strict-server seam (a wrapper root starved
// before its `$1` closure, the write reachable only through the dispatch root —
// the R7/R8 shape), and a mix of boundary effects (classified writes, reads,
// opaque db-call writes, bus publishes, dynamic effects), some on concurrent
// edges. It is deterministic in r, so the corpus is reproducible.
func genGraph(r *rand.Rand) *graph.Graph {
	const mod = "example.com/svc/internal/"
	pkgs := []string{"api", "app", "handler", "store", "worker", "auth"}

	n := 5 + r.Intn(14)
	fqns := make([]string, 0, n)
	nodes := make([]graph.Node, 0, n)
	for i := 0; i < n; i++ {
		pkg := mod + pkgs[r.Intn(len(pkgs))]
		var fqn string
		switch r.Intn(3) {
		case 0:
			fqn = fmt.Sprintf("(*%s.T%d).M", pkg, i)
		case 1:
			fqn = fmt.Sprintf("%s.F%d", pkg, i)
		default:
			fqn = fmt.Sprintf("%s.F%d$1", pkg, i) // an anonymous closure
		}
		fqns = append(fqns, fqn)
		nodes = append(nodes, graph.Node{FQN: fqn, Sig: "func() error", Tier: 1})
	}

	var edges []graph.Edge
	addEdge := func(from, to string) {
		edges = append(edges, graph.Edge{From: from, To: to, Tier: 2, Concurrent: r.Intn(100) < 12})
	}
	// Forward, roughly-DAG structure: a later node may be called by an earlier one
	// (leaving 0..k roots), with occasional extra cross edges (and rare cycles).
	for i := 1; i < n; i++ {
		if r.Intn(100) < 70 {
			addEdge(fqns[r.Intn(i)], fqns[i])
		}
		if r.Intn(100) < 25 {
			if j := r.Intn(n); j != i {
				addEdge(fqns[j], fqns[i])
			}
		}
	}

	// Optional strict-server seam: a wrapper root whose static reach stops before
	// its own `$1` closure (only the dispatch root reaches the closure), with a
	// write reachable past the seam — the topology R7/R8 turned on.
	if n >= 3 && r.Intn(100) < 45 {
		wrapper := fmt.Sprintf("(*%sapi.W%d).Create", mod, r.Intn(1<<20))
		closure := wrapper + "$1"
		dispatch := mod + "api.HandlerWithOptions$1"
		nodes = append(nodes,
			graph.Node{FQN: wrapper, Sig: "func()", Tier: 1},
			graph.Node{FQN: closure, Sig: "func()", Tier: 1},
			graph.Node{FQN: dispatch, Sig: "func()", Tier: 1})
		addEdge(dispatch, closure)
		addEdge(closure, fqns[r.Intn(len(fqns))]) // the closure reaches into the body
		fqns = append(fqns, wrapper, closure, dispatch)
	}

	// Boundary effects on a random subset of functions.
	for _, fqn := range fqns {
		if r.Intn(100) < 45 {
			edges = append(edges, graph.Edge{
				From: fqn, To: randEffect(r), Tier: 1,
				Boundary: "outbound-sync", Concurrent: r.Intn(100) < 12,
			})
		}
	}

	// Optional composition root: a `cmd/…/svc.main` entrypoint that reaches into
	// the body AND makes its own startup writes (migrations/seeding). This is the
	// M-9 shape: main is a caller-less entrypoint but NOT a route, so IsRoute must
	// exclude it in the proposers and the enforcer alike. Half the time record the
	// AUTHORITATIVE CompositionRoots field; half leave it unset (a pre-field graph)
	// to exercise the structural `.main` fallback. If main were charged as a route,
	// its startup writes would inflate proposeBudget or (with a tight enough budget)
	// make init emit a baseline that fails its own gate — the self-clean break the
	// enforcing test above would catch.
	var roots []string
	if r.Intn(100) < 50 {
		mainPkg := "example.com/svc/cmd/svc"
		mainFQN := mainPkg + ".main"
		nodes = append(nodes, graph.Node{FQN: mainFQN, Sig: "func()", Tier: 3})
		if len(fqns) > 0 {
			addEdge(mainFQN, fqns[r.Intn(len(fqns))])
		}
		// A couple of direct startup writes off main only.
		edges = append(edges,
			graph.Edge{From: mainFQN, To: "boundary:db INSERT schema_migrations", Tier: 1, Boundary: "outbound-sync"},
			graph.Edge{From: mainFQN, To: "boundary:db UPDATE seed_data", Tier: 1, Boundary: "outbound-sync"})
		if r.Intn(100) < 50 {
			roots = []string{mainPkg}
		}
	}

	return &graph.Graph{Algo: "vta", Nodes: dedupeNodes(nodes), Edges: edges, CompositionRoots: roots}
}

// randEffect returns one boundary effect label spanning the classifier's whole
// surface: classified writes, a read, opaque db-call writes (non-constant SQL the
// labeler cannot read as a write), bus publishes, and a dynamic effect.
func randEffect(r *rand.Rand) string {
	tables := []string{"users", "orders", "outbox", "events"}
	tbl := tables[r.Intn(len(tables))]
	switch r.Intn(8) {
	case 0:
		return "boundary:db INSERT " + tbl
	case 1:
		return "boundary:db UPDATE " + tbl
	case 2:
		return "boundary:db DELETE " + tbl
	case 3:
		return "boundary:db SELECT " + tbl
	case 4:
		return "boundary:db call" // opaque: non-constant SQL
	case 5:
		return "boundary:db ExecContext" // opaque: method-named fallback
	case 6:
		return "boundary:bus PUBLISH topic." + tbl
	default:
		return "boundary:bus PUBLISH <dynamic>"
	}
}

// dedupeNodes keeps the first node per FQN — a generated seam can re-add a shared
// dispatch root, and NewIndex maps by FQN, so a clean slice keeps the dump honest.
func dedupeNodes(in []graph.Node) []graph.Node {
	seen := map[string]bool{}
	out := in[:0]
	for _, n := range in {
		if seen[n.FQN] {
			continue
		}
		seen[n.FQN] = true
		out = append(out, n)
	}
	return out
}

// dumpGraph renders a generated graph compactly so a failing seed is debuggable
// from the test log alone (no need to re-run a generator by hand).
func dumpGraph(g *graph.Graph) string {
	var b strings.Builder
	ns := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		ns[i] = n.FQN
	}
	sort.Strings(ns)
	b.WriteString("nodes:\n")
	for _, n := range ns {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	es := make([]string, len(g.Edges))
	for i, e := range g.Edges {
		tag := ""
		if e.Concurrent {
			tag = " [concurrent]"
		}
		es[i] = fmt.Sprintf("  %s -> %s%s", e.From, e.To, tag)
	}
	sort.Strings(es)
	b.WriteString("edges:\n")
	b.WriteString(strings.Join(es, "\n"))
	return b.String()
}
