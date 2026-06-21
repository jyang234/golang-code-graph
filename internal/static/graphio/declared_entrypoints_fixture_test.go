package graphio_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
)

func callbacksvcDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "callbacksvc")
}

// TestDeclaredEntrypointsRecoverConesEndToEnd is the end-to-end guard for the
// declared-entrypoints feature, on the callbacksvc fixture — the manager-holds-
// handler idiom (a library-dispatched callback whose handler threads through a
// struct field) plus a `go`-launched background worker.
//
// It pins, on real analyzer output (not a hand-built graph), that:
//   - WITHOUT the declarations, root discovery cannot reach the callback (the
//     value-flow wall orphans it) — the baseline the feature exists to fix;
//   - WITH the declarations (driven from .flowmap.yaml), the callback and worker
//     are rooted, disclosed AS declared (kind "callback"/"worker", Name = the
//     config reference), so a reader can tell a declared entry from a discovered
//     route;
//   - the callback's effect cone (a DB INSERT) and the worker's (a DB read) are
//     now REACHABLE in the graph — the recovery is the point, not the label.
func TestDeclaredEntrypointsRecoverConesEndToEnd(t *testing.T) {
	dir := callbacksvcDir()
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	const (
		callbackFn = "(*example.com/callbacksvc/internal/inbound.Handler).Handle"
		workerFn   = "(*example.com/callbacksvc/internal/reconciler.Reconciler).Start"
		insertFn   = "(*example.com/callbacksvc/internal/store.Store).Insert"
		scanFn     = "(*example.com/callbacksvc/internal/store.Store).Scan"
		callbackEP = "example.com/callbacksvc/internal/inbound#Handle"
		workerEP   = "example.com/callbacksvc/internal/reconciler#Start"
	)

	// Baseline: re-run root discovery WITHOUT the declared entrypoints (only the
	// registrar hints config would otherwise produce). The callback must be orphaned
	// — proving the recovery below is the declaration's doing, not incidental
	// reachability.
	base := roots.Discover(res.Program, analyze.Registrars(res.Config))
	for _, r := range base.Roots {
		if r.FQN() == callbackFn {
			t.Fatalf("callback %s was rooted WITHOUT a declaration; the fixture no longer reproduces the value-flow wall", callbackFn)
		}
	}

	// With declarations (res came through analyze.Analyze, so the .flowmap.yaml
	// entrypoints drove it): callback and worker are rooted with declared provenance.
	gotEP := map[string]string{} // name -> kind
	for _, ep := range g(t, res).Entrypoints {
		gotEP[ep.Name] = ep.Kind
	}
	if gotEP[callbackEP] != "callback" {
		t.Errorf("callback entrypoint %q kind = %q, want %q", callbackEP, gotEP[callbackEP], "callback")
	}
	if gotEP[workerEP] != "worker" {
		t.Errorf("worker entrypoint %q kind = %q, want %q", workerEP, gotEP[workerEP], "worker")
	}

	// The recovered cones: the callback's DB INSERT and the worker's DB read are now
	// reachable nodes, and a classified db write edge is present.
	graph := g(t, res)
	nodes := map[string]bool{}
	for _, n := range graph.Nodes {
		nodes[n.FQN] = true
	}
	for _, want := range []string{callbackFn, insertFn, workerFn, scanFn} {
		if !nodes[want] {
			t.Errorf("expected %s reachable in the graph after rooting; it is missing", want)
		}
	}
	sawDBWrite := false
	for _, e := range graph.Edges {
		if e.From == insertFn && strings.HasPrefix(e.To, "boundary:db ") {
			sawDBWrite = true
		}
	}
	if !sawDBWrite {
		t.Errorf("expected a db boundary edge from %s (the recovered callback effect); edges = %+v", insertFn, graph.Edges)
	}
}

// g builds the graph view for res, failing the test on error.
func g(t *testing.T, res *analyze.Result) *graphio.Graph {
	t.Helper()
	graph, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	return graph
}
