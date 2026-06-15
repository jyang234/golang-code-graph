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
	r := frontier.Summarize(g.Frontier, len(g.Entrypoints))

	if r.StarvedEntrypoints != 3 || r.AttributionLoss != 1.0 {
		t.Errorf("attribution loss: got %d/%d starved (%.2f), want 3/3 (1.00)",
			r.StarvedEntrypoints, r.Entrypoints, r.AttributionLoss)
	}
	if r.Counts[frontier.BinB] != 6 || r.Counts[frontier.BinA] != 1 || r.Counts[frontier.BinB2] != 1 {
		t.Errorf("bins: got A=%d B=%d B2=%d C=%d, want A=1 B=6 B2=1 C=0",
			r.Counts[frontier.BinA], r.Counts[frontier.BinB], r.Counts[frontier.BinB2], r.Counts[frontier.BinC])
	}
	if r.ReclaimableShare < 0.70 {
		t.Errorf("reclaimable share %.2f, want >= 0.70 (B dominates the strict-server frontier)", r.ReclaimableShare)
	}

	want := map[string]frontier.Bin{
		"severed-closure":    frontier.BinB,
		"starved-entrypoint": frontier.BinB,
		"opaque-db":          frontier.BinB2,
		"dynamic-bus":        frontier.BinA,
	}
	got := map[string]frontier.Bin{}
	for _, m := range g.Frontier {
		got[m.Kind] = frontier.Bin(m.Bin)
	}
	for kind, bin := range want {
		if got[kind] != bin {
			t.Errorf("expected a %q marker in bin %s; section = %+v", kind, bin, g.Frontier)
		}
	}
}

// The negative control: non-strict oapisvc registers wrapper methods directly, so
// there is NO dispatch seam. Its one empty-stub route must NOT be flagged — a no-op
// handler owns no severed effect-bearing closure. Proves the classifier does not
// cry seam on every effect-free route, end to end.
func TestNonStrictFrontierHasNoSeam(t *testing.T) {
	g := buildFixture(t, "oapisvc")
	r := frontier.Summarize(g.Frontier, len(g.Entrypoints))
	if r.StarvedEntrypoints != 0 || r.AttributionLoss != 0 {
		t.Errorf("non-strict oapisvc must show no attribution loss; got %d starved (%.2f)",
			r.StarvedEntrypoints, r.AttributionLoss)
	}
	for _, m := range g.Frontier {
		if m.Kind == "severed-closure" || m.Kind == "starved-entrypoint" {
			t.Errorf("non-strict service must not produce a seam marker, got %+v", m)
		}
	}
}
