package frontier_test

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

func fixtureDir(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", name)
}

func classifyFixture(t *testing.T, name string) *frontier.Report {
	t.Helper()
	res, err := analyze.Analyze(fixtureDir(name), callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return frontier.Classify(g)
}

// strictsvc is the seam topology: every route is severed from its effects, so the
// inventory must report total attribution loss and a B-dominated frontier — the
// measurement that motivates the reclaimer. This pins the headline numbers from
// docs/design/frontier-instrumentation-plan.md so they are CI-guarded; when a
// reclaimer lands, attribution loss drops and this test is updated (the win).
func TestClassifyStrictServerSeam(t *testing.T) {
	r := classifyFixture(t, "strictsvc")

	if r.StarvedEntrypoints != 3 || r.AttributionLoss != 1.0 {
		t.Errorf("attribution loss: got %d/%d starved (%.2f), want 3/3 (1.00)",
			r.StarvedEntrypoints, r.Entrypoints, r.AttributionLoss)
	}
	if r.Counts[frontier.BinB] != 6 || r.Counts[frontier.BinA] != 1 || r.Counts[frontier.BinB2] != 1 {
		t.Errorf("bins: got A=%d B=%d B2=%d C=%d, want A=1 B=6 B2=1 C=0",
			r.Counts[frontier.BinA], r.Counts[frontier.BinB], r.Counts[frontier.BinB2], r.Counts[frontier.BinC])
	}
	// The reclaimable share is the design's ~80% claim made concrete.
	if r.ReclaimableShare < 0.70 {
		t.Errorf("reclaimable share %.2f, want >= 0.70 (B dominates the strict-server frontier)", r.ReclaimableShare)
	}

	want := map[string]frontier.Bin{
		"severed-closure":    frontier.BinB,  // the per-handler $1 dispatch seam
		"starved-entrypoint": frontier.BinB,  // the route severed from its effects
		"opaque-db":          frontier.BinB2, // the runtime-SQL write
		"dynamic-bus":        frontier.BinA,  // the runtime topic
	}
	got := map[string]frontier.Bin{}
	for _, m := range r.Markers {
		got[m.Kind] = m.Bin
	}
	for kind, bin := range want {
		if got[kind] != bin {
			t.Errorf("expected a %q marker in bin %s; markers = %+v", kind, bin, r.Markers)
		}
	}
}

// The negative control: non-strict oapisvc registers wrapper methods directly, so
// there is NO dispatch seam. Its one empty-stub route (GetLoanApplicationStatus
// reaches no effect) must NOT be flagged — a no-op handler owns no severed
// effect-bearing closure, so it is not a confirmed seam and there is nothing to
// reclaim. Proves the classifier does not cry seam on every effect-free route.
func TestClassifyNonStrictHasNoSeam(t *testing.T) {
	r := classifyFixture(t, "oapisvc")
	if r.StarvedEntrypoints != 0 || r.AttributionLoss != 0 {
		t.Errorf("non-strict oapisvc must show no attribution loss; got %d starved (%.2f)",
			r.StarvedEntrypoints, r.AttributionLoss)
	}
	for _, m := range r.Markers {
		if m.Kind == "severed-closure" || m.Kind == "starved-entrypoint" {
			t.Errorf("non-strict service must not produce a seam marker, got %+v", m)
		}
	}
}

// The inventory is a pure function of the graph (rule R3 / the determinism
// doctrine): classifying the same graph twice yields identical reports.
func TestClassifyDeterministic(t *testing.T) {
	res, err := analyze.Analyze(fixtureDir("strictsvc"), callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	a, b := frontier.Classify(g), frontier.Classify(g)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("classification is not deterministic:\n a=%+v\n b=%+v", a, b)
	}
}
