package graphio_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

func eventbussvcDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "eventbussvc")
}

// TestRollupCompositionRootWiringEndToEnd is the end-to-end guard for the
// composition-root wiring classification, run against the eventbussvc fixture — a
// hand-written stand-in for oapi-codegen's strict server built WITH error-handler
// options. The fixture's `main` injects two closures (newHandler$1/$2) into the
// generated `api` handler, which invokes them on the error path, producing the
// back-edge api.strictHandler.<Op> -> eventbussvc.newHandler$N — the exact pattern
// the field report observed on event-bus. The DEFAULT (RTA) build resolves the
// func-value calls, so no --reclaim/--algo is needed.
//
// It pins, on real analyzer output (not a hand-built graph), that:
//   - the `package main` component is flagged role="composition_root";
//   - the api -> main back-edge is classified "wiring", NOT a domain "call";
//   - the api -> server domain dependency is still a plain "call" (the wiring
//     reclassification does not swallow real dependencies);
//   - no edge points the wrong way as a CODE call into the composition root (the
//     C3 inversion the wiring class exists to prevent).
func TestRollupCompositionRootWiringEndToEnd(t *testing.T) {
	res, err := analyze.Analyze(eventbussvcDir())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	r := g.RollupByPackage()

	const (
		root   = "example.com/eventbussvc"
		api    = "example.com/eventbussvc/api"
		server = "example.com/eventbussvc/server"
	)

	var rootComp *graphio.Component
	for i := range r.Components {
		if r.Components[i].Package == root {
			rootComp = &r.Components[i]
		}
	}
	if rootComp == nil {
		t.Fatalf("composition-root component %q missing from rollup: %+v", root, r.Components)
	}
	if rootComp.Role != graphio.RollupRoot {
		t.Errorf("component %q Role = %q, want %q", root, rootComp.Role, graphio.RollupRoot)
	}

	var sawWiring, sawDomainCall bool
	for _, e := range r.Edges {
		switch {
		case e.From == api && e.To == root:
			if e.Kind != graphio.RollupWiring {
				t.Errorf("api->main back-edge Kind = %q, want %q (wiring): %+v", e.Kind, graphio.RollupWiring, e)
			}
			sawWiring = true
		case e.From == api && e.To == server:
			if e.Kind != graphio.RollupCall {
				t.Errorf("api->server domain edge Kind = %q, want %q (call): %+v", e.Kind, graphio.RollupCall, e)
			}
			sawDomainCall = true
		}
		// No edge may read as a CODE call INTO the composition root — that is the
		// architecture inversion the wiring class exists to prevent.
		if e.To == root && e.Kind == graphio.RollupCall {
			t.Errorf("an edge reads as a domain call into the composition root (inverted): %+v", e)
		}
	}
	if !sawWiring {
		t.Errorf("expected an api->main wiring back-edge; rollup edges = %+v", r.Edges)
	}
	if !sawDomainCall {
		t.Errorf("expected an api->server domain call; rollup edges = %+v", r.Edges)
	}
}
