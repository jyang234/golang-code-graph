package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestMapInternalOutcomes pins the four §7 outcomes of the internal-span map, each
// the honest classification the severance walk depends on: a tag that reconciles
// to exactly one node is `mapped`; a tag keying a function the graph lacks is the
// sharp `absent-from-graph`; no tag is `untagged`; a ⊥ tag (closure/generic) is
// `untagged` with the canonFQN reason — never guessed onto a node.
func TestMapInternalOutcomes(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{Nodes: []graph.Node{
		{FQN: "ex.com/svc/store.New"},
		{FQN: "(*ex.com/svc/store.Loans).Purge"},
	}})
	nx := buildNodeIndex(ix)

	span := func(tag string) *ir.CanonicalSpan {
		return &ir.CanonicalSpan{Op: "x", Kind: ir.KindInternal, Attrs: map[string]string{FQNTagKey: tag}}
	}
	cases := []struct {
		name, tag   string
		wantOutcome string
		wantNode    string
	}{
		// runtime spelling of the pointer method reconciles to the ssa node.
		{"mapped method", "ex.com/svc/store.(*Loans).Purge", MapMapped, "(*ex.com/svc/store.Loans).Purge"},
		{"mapped func", "ex.com/svc/store.New", MapMapped, "ex.com/svc/store.New"},
		{"absent-from-graph", "ex.com/svc/store.(*Loans).Wipe", MapAbsentFromGraph, ""},
		{"untagged ⊥ closure", "ex.com/svc/store.Purge.func1", MapUntagged, ""},
		{"untagged ⊥ generic", "ex.com/svc/store.Map[go.shape.int]", MapUntagged, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := nx.mapInternal(span(c.tag))
			if a.Outcome != c.wantOutcome {
				t.Errorf("Outcome = %q (%s), want %q", a.Outcome, a.Reason, c.wantOutcome)
			}
			if a.Node != c.wantNode {
				t.Errorf("Node = %q, want %q", a.Node, c.wantNode)
			}
			if c.wantOutcome == MapAbsentFromGraph && a.Tag != c.tag {
				t.Errorf("absent-from-graph dropped the tag identity: %q", a.Tag)
			}
		})
	}

	// An untagged span (no flowmap.fqn at all) is untagged, not a guess.
	if a := nx.mapInternal(&ir.CanonicalSpan{Op: "x", Kind: ir.KindInternal}); a.Outcome != MapUntagged {
		t.Errorf("missing tag: Outcome = %q, want %q", a.Outcome, MapUntagged)
	}
}

// TestMapInternalAmbiguousCollision is the §7 fail-closed property 2: when two
// nodes canonFQN to the SAME key (a collision the key cannot resolve), a span
// keying to it is `ambiguous` ⇒ ⊥, never one of the two guessed. A value method
// and a package-level func can collide because the value-method runtime spelling
// is indistinguishable from a package func in a sub-package — exactly the
// ambiguity canonFQN refuses to resolve.
func TestMapInternalAmbiguousCollision(t *testing.T) {
	// Both of these canonFQN to {Pkg: ex.com/p, Recv: T, Name: M}: the ssa value
	// method and a (contrived) node spelled to collide.
	ix := graph.NewIndex(&graph.Graph{Nodes: []graph.Node{
		{FQN: "(ex.com/p.T).M"}, // value method -> {ex.com/p, T, false, M}
		{FQN: "ex.com/p.T.M"},   // package func T.M in pkg ex.com/p -> same key
	}})
	nx := buildNodeIndex(ix)
	a := nx.mapInternal(&ir.CanonicalSpan{Attrs: map[string]string{FQNTagKey: "ex.com/p.T.M"}})
	if a.Outcome != MapAmbiguous {
		t.Fatalf("Outcome = %q (%s), want %q on a key collision", a.Outcome, a.Reason, MapAmbiguous)
	}
	if a.Node != "" {
		t.Errorf("ambiguous map returned a guessed node %q", a.Node)
	}
}
