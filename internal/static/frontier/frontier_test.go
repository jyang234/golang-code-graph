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
