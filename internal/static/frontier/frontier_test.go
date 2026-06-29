package frontier_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

// buildFixture runs the real analyzer + graphio.Build on a fixture, returning the
// graph with its embedded frontier section. These tests are the end-to-end proof
// that (a) the analyzer produces the seam shape and (b) Build classifies and
// embeds it — the producer→section path the unit table cannot cover.
func buildFixture(t *testing.T, name string) *graphio.Graph {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", name)
	res, err := analyze.Analyze(dir, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return g
}

// strictsvc is the seam topology: every route is severed from its effects, so the
// embedded section must report total attribution loss and a B-dominated frontier.
// This pins the headline numbers from docs/design/frontier-instrumentation-plan.md
// AND proves graphio.Build embeds them; when a reclaimer lands, attribution loss
// drops and this test is updated (the win).
func TestStrictServerFrontierSection(t *testing.T) {
	g := buildFixture(t, "strictsvc")
	r := frontier.Summarize(graphio.ClassifyFrontier(g), len(g.Entrypoints))

	if r.StarvedEntrypoints != 3 || r.AttributionLoss != 1.0 {
		t.Errorf("attribution loss: got %d/%d starved (%.2f), want 3/3 (1.00)",
			r.StarvedEntrypoints, r.Entrypoints, r.AttributionLoss)
	}
	// Every severed route here is CONFIRMED (its own $1 closure), so there is no
	// unconfirmed remainder — attribution loss is exact, not a lower bound.
	if len(r.UnconfirmedRoutes) != 0 {
		t.Errorf("strictsvc routes are all confirmed-severed; want 0 unconfirmed, got %v", r.UnconfirmedRoutes)
	}
	if r.Counts[frontier.BinB] != 6 || r.Counts[frontier.BinA] != 1 || r.Counts[frontier.BinB2] != 1 {
		t.Errorf("bins: got A=%d B=%d B2=%d C=%d, want A=1 B=6 B2=1 C=0",
			r.Counts[frontier.BinA], r.Counts[frontier.BinB], r.Counts[frontier.BinB2], r.Counts[frontier.BinC])
	}
	if r.ReclaimableShare < 0.70 {
		t.Errorf("reclaimable share %.2f, want >= 0.70 (B dominates the strict-server frontier)", r.ReclaimableShare)
	}

	if g.Frontier == nil {
		t.Fatal("strictsvc must carry a frontier section")
	}
	want := map[string]frontier.Bin{
		"severed-closure":    frontier.BinB,
		"starved-entrypoint": frontier.BinB,
		"opaque-db":          frontier.BinB2,
		"dynamic-bus":        frontier.BinA,
	}
	got := map[string]frontier.Bin{}
	for _, m := range g.Frontier.Markers {
		got[m.Kind] = frontier.Bin(m.Bin)
	}
	for kind, bin := range want {
		if got[kind] != bin {
			t.Errorf("expected a %q marker in bin %s; section = %+v", kind, bin, g.Frontier)
		}
	}
}

// TestUnresolvedCallFrontierDedup pins the conditional exclusion of UnresolvedCall:
// it is deduped ONLY against a structural marker at the SAME site (where it would
// double-count a seam already counted), and is otherwise emitted so the frontier
// section stays complete. Driven through frontier.Classify with a minimal Input so
// both branches are exercised without needing two whole fixtures.
func TestUnresolvedCallFrontierDedup(t *testing.T) {
	// Coinciding: X is a starved-entrypoint (reaches no effect itself but owns the
	// severed, effect-bearing closure X$1). An UnresolvedCall at X is redundant.
	coinciding := frontier.Input{
		Nodes:       []string{"X", "X$1"},
		Edges:       []frontier.InEdge{{From: "X$1", To: "boundary:db DELETE ledger"}},
		Entrypoints: []frontier.InEntry{{Fn: "X", Name: "GET /x"}},
		BlindSpots:  []frontier.InBlindSpot{{Kind: "UnresolvedCall", Site: "X"}},
	}
	for _, m := range frontier.Classify(&coinciding).Markers {
		if m.Kind == "UnresolvedCall" {
			t.Errorf("UnresolvedCall at a structurally-marked site must be deduped, got marker %+v", m)
		}
	}

	// Standalone: Z carries an UnresolvedCall but no structural marker (a func-value
	// call outside any recognized dispatch seam). It MUST be disclosed, as BinA.
	standalone := frontier.Input{
		Nodes:      []string{"Z"},
		BlindSpots: []frontier.InBlindSpot{{Kind: "UnresolvedCall", Site: "Z"}},
	}
	var got *frontier.Marker
	for _, m := range frontier.Classify(&standalone).Markers {
		if m.Kind == "UnresolvedCall" {
			mm := m
			got = &mm
		}
	}
	if got == nil {
		t.Fatal("standalone UnresolvedCall (no structural marker) must be disclosed in the frontier, got none")
	}
	if got.Site != "Z" || got.Bin != frontier.BinA {
		t.Errorf("standalone UnresolvedCall marker = %+v, want Site=Z Bin=A", *got)
	}
}

// TestMiddlewareReclaimableBinsB pins the §8 prediction: a standalone UnresolvedCall whose
// site the middleware-chain reclaimer proves empty (in.MiddlewareReclaimable) is binned B
// (reclaimable by --reclaim-middleware), while one absent from the set stays A (irreducible).
// Same Input shape as the standalone-dedup case, so only the new bin override is exercised.
func TestMiddlewareReclaimableBinsB(t *testing.T) {
	in := frontier.Input{
		Nodes: []string{"Apply", "DynApply"},
		BlindSpots: []frontier.InBlindSpot{
			{Kind: "UnresolvedCall", Site: "Apply"},    // a provably-empty middleware loop
			{Kind: "UnresolvedCall", Site: "DynApply"}, // a dynamic one — not reclaimable
		},
		MiddlewareReclaimable: []string{"Apply"},
	}
	bins := map[string]frontier.Bin{}
	for _, m := range frontier.Classify(&in).Markers {
		if m.Kind == "UnresolvedCall" {
			bins[m.Site] = frontier.Bin(m.Bin)
		}
	}
	if bins["Apply"] != frontier.BinB {
		t.Errorf("a middleware-reclaimable UnresolvedCall must bin B, got %q", bins["Apply"])
	}
	if bins["DynApply"] != frontier.BinA {
		t.Errorf("a non-reclaimable UnresolvedCall must stay A, got %q", bins["DynApply"])
	}
}

// A scoped (--entry) build carries NO frontier section: it is a whole-service
// disclosure, and a scoped cone drops entrypoints and prunes effect paths, so its
// starvation / attribution-loss signal would be a scoping artifact. Gating it on
// the unscoped build matches the Obligations/EffectOrder convention.
func TestScopedBuildHasNoFrontier(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "strictsvc")
	res, err := analyze.Analyze(dir, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	g, err := graphio.Build(res, "POST /eventTypeTemplates")
	if err != nil {
		t.Fatalf("scoped build: %v", err)
	}
	if g.Frontier != nil {
		t.Errorf("scoped build must carry no frontier section; got %+v", g.Frontier)
	}
}

// The negative control: non-strict oapisvc registers wrapper methods directly, so
// there is NO dispatch seam. Its empty-stub route (reaches no effect) must NOT be a
// CONFIRMED seam marker — but it IS honestly disclosed as UNCONFIRMED (the third
// state), not silently dropped. Proves the classifier neither cries seam on an
// effect-free route nor hides it.
func TestNonStrictFrontierHasNoSeam(t *testing.T) {
	g := buildFixture(t, "oapisvc")
	r := frontier.Summarize(graphio.ClassifyFrontier(g), len(g.Entrypoints))
	if r.StarvedEntrypoints != 0 || r.AttributionLoss != 0 {
		t.Errorf("non-strict oapisvc must show no CONFIRMED attribution loss; got %d starved (%.2f)",
			r.StarvedEntrypoints, r.AttributionLoss)
	}
	// The no-op stub reaches no effect, so it is disclosed as unconfirmed (not silent).
	if len(r.UnconfirmedRoutes) == 0 {
		t.Errorf("the effect-free stub route must be disclosed as unconfirmed, not dropped")
	}
	if g.Frontier != nil {
		for _, m := range g.Frontier.Markers {
			if m.Kind == "severed-closure" || m.Kind == "starved-entrypoint" {
				t.Errorf("non-strict service must not produce a seam marker, got %+v", m)
			}
		}
	}
}
