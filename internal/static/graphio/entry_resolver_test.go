package graphio

import (
	"errors"
	"testing"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/roots"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

// M-12: a --entry name that resolves to more than one distinct root handler must
// FAIL the build, not root the scope at whichever was registered first. Before the
// fix, Build's rootByName returned the first match arbitrarily while the render
// path's resolveRoot failed closed — the two --entry resolvers disagreed on
// ambiguity. Now Build refuses via resolveEntryRoot.
func TestBuildAmbiguousEntryFailsClosed(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// Two distinct real root functions, forced under one Name.
	var funcs []*ssa.Function
	seen := map[*ssa.Function]bool{}
	for _, r := range res.Roots.Roots {
		if r.Func != nil && !seen[r.Func] {
			seen[r.Func] = true
			funcs = append(funcs, r.Func)
		}
		if len(funcs) == 2 {
			break
		}
	}
	if len(funcs) < 2 {
		t.Skip("fixture has fewer than two distinct roots; cannot construct ambiguity")
	}

	res.Roots.Roots = []roots.Root{
		{Func: funcs[0], Name: "ambiguous-entry"},
		{Func: funcs[1], Name: "ambiguous-entry"},
	}

	// Build must refuse with the typed ambiguity error.
	if _, err := Build(res, "ambiguous-entry"); err == nil {
		t.Fatal("Build accepted an ambiguous --entry; expected EntryAmbiguousError")
	} else {
		var amb *EntryAmbiguousError
		if !errors.As(err, &amb) {
			t.Fatalf("Build error = %v, want *EntryAmbiguousError", err)
		}
		if len(amb.Fns) != 2 {
			t.Errorf("ambiguity error should name both handlers, got %v", amb.Fns)
		}
	}

	// The resolver itself: ambiguous → error; exactly one → the func; missing → nil,nil.
	if _, err := resolveEntryRoot(res, "ambiguous-entry"); err == nil {
		t.Error("resolveEntryRoot accepted an ambiguous name")
	}
	res.Roots.Roots = []roots.Root{{Func: funcs[0], Name: "single"}}
	if fn, err := resolveEntryRoot(res, "single"); err != nil || fn != funcs[0] {
		t.Errorf("resolveEntryRoot(single) = (%v, %v), want (funcs[0], nil)", fn, err)
	}
	if fn, err := resolveEntryRoot(res, "nope"); err != nil || fn != nil {
		t.Errorf("resolveEntryRoot(missing) = (%v, %v), want (nil, nil)", fn, err)
	}
}

// The render-side twin: (*Graph).resolveRoot must ALSO fail closed when two
// entrypoints share a Name but resolve to different handlers — symmetric with the
// build-time resolver (M-12).
func TestResolveRootAmbiguousExactNameFailsClosed(t *testing.T) {
	g := &Graph{
		Entrypoints: []Entrypoint{
			{Kind: "http", Name: "GET /x", Fn: "pkg.HandlerA"},
			{Kind: "http", Name: "GET /x", Fn: "pkg.HandlerB"},
		},
	}
	if fn, ok := g.resolveRoot("GET /x"); ok {
		t.Errorf("resolveRoot resolved an ambiguous exact Name to %q; expected fail-closed", fn)
	}

	// One handler under the Name resolves fine.
	g.Entrypoints = []Entrypoint{{Kind: "http", Name: "GET /x", Fn: "pkg.HandlerA"}}
	if fn, ok := g.resolveRoot("GET /x"); !ok || fn != "pkg.HandlerA" {
		t.Errorf("resolveRoot(unambiguous) = (%q, %v), want (pkg.HandlerA, true)", fn, ok)
	}
}
