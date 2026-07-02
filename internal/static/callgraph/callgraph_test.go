package callgraph_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func buildFixtureGraph(t *testing.T) *callgraph.Graph {
	t.Helper()
	prog, err := statictest.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rs := roots.Discover(prog, statictest.Registrars())
	g, err := callgraph.Build(prog, rs, callgraph.Options{})
	if err != nil {
		t.Fatalf("callgraph build: %v", err)
	}
	return g
}

func TestRTAReachability(t *testing.T) {
	g := buildFixtureGraph(t)
	if g.Algo != callgraph.AlgoRTA {
		t.Fatalf("algo = %q, want rta", g.Algo)
	}

	const (
		evaluate        = "(*example.com/loansvc/internal/origination.Evaluator).Evaluate"
		selectApplicant = "(*example.com/loansvc/internal/store.Loans).SelectApplicant"
		auditLog        = "(*example.com/loansvc/internal/origination.Evaluator).auditLog"
		insertAudit     = "(*example.com/loansvc/internal/store.Loans).InsertAudit"
		markPaid        = "(*example.com/loansvc/internal/store.Loans).MarkPaid"
	)
	// Evaluate is reachable only through the HTTP handler root, not from main —
	// which is the whole point of synthetic roots.
	mustReach(t, g, evaluate)
	// The errgroup-concurrent pair: the DB read leg.
	mustReach(t, g, selectApplicant)
	// The fire-and-forget goroutine target and what it calls.
	mustReach(t, g, auditLog)
	mustReach(t, g, insertAudit)
	// The consumer root reaches its DB mutate.
	mustReach(t, g, markPaid)
}

// TestNodeOrderDeterministic pins the M-20 tie-break: node order is byte-identical
// across repeated builds even though the fixture now contains generic instances,
// whose display FQN (fn.RelString) is documented non-unique. An FQN-only sort over
// the map-iteration-ordered node set would vary run-to-run on such a tie; the
// InstanceDiscriminator secondary key makes the order total. (finalize panics on a
// genuine collision, so a passing build also proves none exists in the fixture.)
func TestNodeOrderDeterministic(t *testing.T) {
	var first []string
	for i := 0; i < 5; i++ {
		g := buildFixtureGraph(t)
		order := make([]string, len(g.Nodes))
		for j, n := range g.Nodes {
			order[j] = n.FQN
		}
		if i == 0 {
			first = order
			continue
		}
		if len(order) != len(first) {
			t.Fatalf("node count varied across builds: %d vs %d", len(order), len(first))
		}
		for j := range order {
			if order[j] != first[j] {
				t.Fatalf("node order varied at %d across builds: %q vs %q", j, order[j], first[j])
			}
		}
	}
}

func TestGenericInstantiationReachable(t *testing.T) {
	g := buildFixtureGraph(t)
	for _, n := range g.Nodes {
		if strings.Contains(n.FQN, "internal/codec.Decode[") {
			return
		}
	}
	t.Fatal("generic instantiation codec.Decode[...] not reachable in call graph")
}

// TestInterfaceResolvesToTwoCallees checks RTA resolved the scoring.Scorer call
// to both concrete implementations — a single caller with edges to both methods.
func TestInterfaceResolvesToTwoCallees(t *testing.T) {
	g := buildFixtureGraph(t)
	const (
		remote = "(*example.com/loansvc/internal/scoring.Remote).Score"
		stub   = "(*example.com/loansvc/internal/scoring.Stub).Score"
	)
	mustReach(t, g, remote)
	mustReach(t, g, stub)

	var caller string
	for _, n := range g.Nodes {
		toRemote, toStub := false, false
		for _, e := range n.Out {
			switch e.Callee.FQN {
			case remote:
				toRemote = true
			case stub:
				toStub = true
			}
		}
		if toRemote && toStub {
			caller = n.FQN
		}
	}
	if caller == "" {
		t.Fatalf("no single caller resolves the interface call to both %s and %s", remote, stub)
	}
	t.Logf("interface call site resolved to both impls from %s", caller)
}

func TestBuildTwiceIdentical(t *testing.T) {
	a := serialize(buildFixtureGraph(t))
	b := serialize(buildFixtureGraph(t))
	if a != b {
		t.Fatalf("call graph not reproducible across builds\n--- first ---\n%s\n--- second ---\n%s",
			head(a), head(b))
	}
}

func TestCHAFallbackWhenNoRoots(t *testing.T) {
	prog, err := statictest.Build()
	if err != nil {
		t.Fatal(err)
	}
	// No roots: RTA cannot run, so Build must fall back to CHA and disclose it.
	g, err := callgraph.Build(prog, &roots.Result{}, callgraph.Options{Algo: callgraph.AlgoRTA})
	if err != nil {
		t.Fatal(err)
	}
	if g.Algo != callgraph.AlgoCHA {
		t.Fatalf("algo = %q, want cha fallback", g.Algo)
	}
	var disclosed bool
	for _, c := range g.Caveats {
		if strings.Contains(c, "fell back") && strings.Contains(c, "cha") {
			disclosed = true
		}
	}
	if !disclosed {
		t.Errorf("CHA fallback not disclosed in caveats: %v", g.Caveats)
	}
	if len(g.Nodes) == 0 {
		t.Error("CHA produced an empty graph")
	}
}

func TestVTARefineReachesTargets(t *testing.T) {
	prog, err := statictest.Build()
	if err != nil {
		t.Fatal(err)
	}
	rs := roots.Discover(prog, statictest.Registrars())
	g, err := callgraph.Build(prog, rs, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("vta build: %v", err)
	}
	if g.Algo != callgraph.AlgoVTA {
		t.Fatalf("algo = %q, want vta", g.Algo)
	}
	// VTA is a refinement, not a regression: the core reachability still holds.
	mustReach(t, g, "(*example.com/loansvc/internal/origination.Evaluator).Evaluate")
	mustReach(t, g, "(*example.com/loansvc/internal/store.Loans).SelectApplicant")
}

func mustReach(t *testing.T, g *callgraph.Graph, fqn string) {
	t.Helper()
	if !g.Reachable(fqn) {
		t.Errorf("not reachable: %s", fqn)
	}
}

// serialize renders the graph as a sorted, stable text form: the node list
// followed by the caller→callee edge list. Two byte-equal serializations mean
// identical node and edge sets.
func serialize(g *callgraph.Graph) string {
	var b strings.Builder
	b.WriteString("# nodes\n")
	for _, n := range g.Nodes {
		fmt.Fprintln(&b, n.FQN)
	}
	b.WriteString("# edges\n")
	var edges []string
	for _, n := range g.Nodes {
		for _, e := range n.Out {
			edges = append(edges, e.Caller.FQN+" -> "+e.Callee.FQN)
		}
	}
	sort.Strings(edges)
	for _, e := range edges {
		fmt.Fprintln(&b, e)
	}
	return b.String()
}

func head(s string) string {
	lines := strings.SplitN(s, "\n", 41)
	if len(lines) > 40 {
		lines = lines[:40]
	}
	return strings.Join(lines, "\n")
}
