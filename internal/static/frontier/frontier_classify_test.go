package frontier_test

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

// These tests exercise the classifier as the pure function it is: hand-authored
// graphio.Graph values, one per marker shape and per NEGATIVE case, asserting the
// exact binning. This is the faithful unit-level complement to the source-fixture
// tests (TestClassifyStrictServerSeam et al.), which prove the real analyzer
// actually emits the seam shape but are slow and cannot deterministically reach
// every bin. The boundary labels and `$N`/FQN shapes here mirror what graphio
// emits — verified against the real strictsvc graph by the end-to-end test — so
// the fixtures are faithful, not fictional.

func node(fqn string) graphio.Node { return graphio.Node{FQN: fqn, Sig: "func()", Tier: 1} }

func edge(from, to string) graphio.Edge {
	b := ""
	if strings.HasPrefix(to, "boundary:") {
		b = "outbound-sync"
	}
	return graphio.Edge{From: from, To: to, Tier: 2, Boundary: b}
}

// build assembles a graph from node FQNs, edges, optional entrypoints, and blind
// spots. Boundary targets in edges are leaf nodes (not declared in nodes).
func build(nodes []string, edges [][2]string, entries []graphio.Entrypoint, blind []blindspots.BlindSpot) *graphio.Graph {
	g := &graphio.Graph{Algo: "vta", Entrypoints: entries, BlindSpots: blind}
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, node(n))
	}
	for _, e := range edges {
		g.Edges = append(g.Edges, edge(e[0], e[1]))
	}
	return g
}

func marker(r *frontier.Report, kind string) (frontier.Marker, bool) {
	for _, m := range r.Markers {
		if m.Kind == kind {
			return m, true
		}
	}
	return frontier.Marker{}, false
}

// FQNs shared across cases.
const (
	wrap  = "(*example.com/svc/internal/api.W).Create"
	clo   = "(*example.com/svc/internal/api.W).Create$1"
	store = "(*example.com/svc/internal/store.S).Del"
)

func TestClassifyMarkerShapes(t *testing.T) {
	cases := []struct {
		name    string
		graph   *graphio.Graph
		present map[string]frontier.Bin // kind -> expected bin (must appear)
		absent  []string                // kinds that must NOT appear
	}{
		{
			name: "severed closure reaching an effect is B",
			graph: build(
				[]string{wrap, clo, store},
				[][2]string{{clo, store}, {store, "boundary:db DELETE provisioning_outbox"}},
				nil, nil),
			present: map[string]frontier.Bin{"severed-closure": frontier.BinB},
		},
		{
			name: "severed closure reaching NO effect is not flagged (benign leaf callback)",
			graph: build(
				[]string{"(*example.com/svc/internal/api.W).Cmp", "(*example.com/svc/internal/api.W).Cmp$1"},
				nil, nil, nil),
			absent: []string{"severed-closure"},
		},
		{
			name: "severed closure whose parent is not a node is not flagged",
			graph: build(
				[]string{clo, store}, // parent `...Create` absent from nodes
				[][2]string{{clo, store}, {store, "boundary:db DELETE x"}},
				nil, nil),
			absent: []string{"severed-closure"},
		},
		{
			name: "entrypoint severed from its own effect-bearing closure is a starved seam (B)",
			graph: build(
				[]string{wrap, clo, store},
				[][2]string{{clo, store}, {store, "boundary:db DELETE x"}},
				[]graphio.Entrypoint{{Kind: "http", Name: "POST /x", Fn: wrap}}, nil),
			present: map[string]frontier.Bin{"starved-entrypoint": frontier.BinB, "severed-closure": frontier.BinB},
		},
		{
			name: "no-op stub entrypoint owning no effect closure is not starved",
			graph: build(
				[]string{"(*example.com/svc/internal/api.W).Health"},
				nil,
				[]graphio.Entrypoint{{Kind: "http", Name: "GET /health", Fn: "(*example.com/svc/internal/api.W).Health"}}, nil),
			absent: []string{"starved-entrypoint"},
		},
		{
			name: "entrypoint reaching an effect directly is not starved",
			graph: build(
				[]string{"(*example.com/svc/internal/api.W).List", store},
				[][2]string{{"(*example.com/svc/internal/api.W).List", store}, {store, "boundary:db SELECT users"}},
				[]graphio.Entrypoint{{Kind: "http", Name: "GET /list", Fn: "(*example.com/svc/internal/api.W).List"}}, nil),
			absent: []string{"starved-entrypoint", "opaque-db"},
		},
		{
			name:    "dynamic bus topic is A",
			graph:   build([]string{wrap}, [][2]string{{wrap, "boundary:bus PUBLISH <dynamic>"}}, nil, nil),
			present: map[string]frontier.Bin{"dynamic-bus": frontier.BinA},
		},
		{
			name:    "dynamic non-bus effect is A",
			graph:   build([]string{wrap}, [][2]string{{wrap, "boundary:<dynamic>"}}, nil, nil),
			present: map[string]frontier.Bin{"dynamic-effect": frontier.BinA},
		},
		{
			name:    "opaque db ExecContext is B2",
			graph:   build([]string{wrap}, [][2]string{{wrap, "boundary:db ExecContext"}}, nil, nil),
			present: map[string]frontier.Bin{"opaque-db": frontier.BinB2},
		},
		{
			name:    "opaque db call is B2",
			graph:   build([]string{wrap}, [][2]string{{wrap, "boundary:db call"}}, nil, nil),
			present: map[string]frontier.Bin{"opaque-db": frontier.BinB2},
		},
		{
			name: "readable db verbs are not opaque",
			graph: build([]string{wrap},
				[][2]string{
					{wrap, "boundary:db DELETE provisioning_outbox"},
					{wrap, "boundary:db SELECT users"},
					{wrap, "boundary:db UPDATE loans"},
				}, nil, nil),
			absent: []string{"opaque-db"},
		},
		{
			name:   "named publish is not dynamic",
			graph:  build([]string{wrap}, [][2]string{{wrap, "boundary:bus PUBLISH orders"}}, nil, nil),
			absent: []string{"dynamic-bus", "dynamic-effect"},
		},
		{
			name:    "HighFanOut blind spot is C",
			graph:   build([]string{wrap}, nil, nil, []blindspots.BlindSpot{{Kind: blindspots.HighFanOut, Site: wrap}}),
			present: map[string]frontier.Bin{string(blindspots.HighFanOut): frontier.BinC},
		},
		{
			name: "reflect/unsafe/cgo/linkname blind spots are A",
			graph: build([]string{wrap}, nil, nil, []blindspots.BlindSpot{
				{Kind: blindspots.Reflect, Site: wrap},
				{Kind: blindspots.Unsafe, Site: "example.com/svc/internal/x"},
				{Kind: blindspots.Cgo, Site: "example.com/svc/internal/y"},
				{Kind: blindspots.Linkname, Site: "example.com/svc/internal/z"},
			}),
			present: map[string]frontier.Bin{
				string(blindspots.Reflect):  frontier.BinA,
				string(blindspots.Unsafe):   frontier.BinA,
				string(blindspots.Cgo):      frontier.BinA,
				string(blindspots.Linkname): frontier.BinA,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := frontier.Classify(tc.graph)
			for kind, bin := range tc.present {
				m, ok := marker(r, kind)
				if !ok {
					t.Errorf("expected a %q marker; markers = %+v", kind, r.Markers)
					continue
				}
				if m.Bin != bin {
					t.Errorf("%q binned %s, want %s", kind, m.Bin, bin)
				}
			}
			for _, kind := range tc.absent {
				if _, ok := marker(r, kind); ok {
					t.Errorf("%q must NOT appear; markers = %+v", kind, r.Markers)
				}
			}
		})
	}
}

// The roll-ups (per-bin counts and the two ratios) over a graph mixing one marker
// of each bin, so a miscount in the aggregation is caught independently of the
// per-marker binning above.
func TestClassifyRollups(t *testing.T) {
	g := build(
		[]string{wrap, clo, store, "(*example.com/svc/internal/api.W).Sync"},
		[][2]string{
			{clo, store},                    // severed closure (B) ...
			{store, "boundary:db DELETE x"}, // ... reaching a classified write
			{"(*example.com/svc/internal/api.W).Sync", "boundary:db ExecContext"},        // opaque write (B2)
			{"(*example.com/svc/internal/api.W).Sync", "boundary:bus PUBLISH <dynamic>"}, // dynamic topic (A)
		},
		[]graphio.Entrypoint{
			{Kind: "http", Name: "POST /x", Fn: wrap},                                        // severed → starved (B)
			{Kind: "http", Name: "POST /sync", Fn: "(*example.com/svc/internal/api.W).Sync"}, // reaches effects → not starved
		},
		[]blindspots.BlindSpot{{Kind: blindspots.HighFanOut, Site: wrap}}, // (C)
	)
	r := frontier.Classify(g)

	if r.Counts[frontier.BinA] != 1 || r.Counts[frontier.BinB] != 2 || r.Counts[frontier.BinB2] != 1 || r.Counts[frontier.BinC] != 1 {
		t.Errorf("counts A=%d B=%d B2=%d C=%d, want 1/2/1/1",
			r.Counts[frontier.BinA], r.Counts[frontier.BinB], r.Counts[frontier.BinB2], r.Counts[frontier.BinC])
	}
	// 2 entrypoints, 1 starved (the severed wrap; Sync reaches its effects).
	if r.Entrypoints != 2 || r.StarvedEntrypoints != 1 || r.AttributionLoss != 0.5 {
		t.Errorf("attribution: %d/%d starved (%.2f), want 1/2 (0.50)",
			r.StarvedEntrypoints, r.Entrypoints, r.AttributionLoss)
	}
	// 5 markers total (severed-closure, starved-entrypoint, opaque-db, dynamic-bus,
	// HighFanOut); B share = 2/5.
	if len(r.Markers) != 5 || r.ReclaimableShare != 2.0/5.0 {
		t.Errorf("markers=%d reclaimable=%.3f, want 5 and 0.400", len(r.Markers), r.ReclaimableShare)
	}
}

// An empty / effect-free graph yields an empty inventory and zero ratios — no
// divide-by-zero, no spurious markers.
func TestClassifyEmptyGraph(t *testing.T) {
	r := frontier.Classify(&graphio.Graph{})
	if len(r.Markers) != 0 || r.ReclaimableShare != 0 || r.AttributionLoss != 0 {
		t.Errorf("empty graph must classify to nothing; got %+v", r)
	}
}
