package analyze_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/signatures"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

// canonicalIR renders the front-end intermediate representation — the loaded
// package set, the discovered roots, and each root's rendered signature — as a
// single deterministic string. It deliberately sorts every collection so the dump
// reflects the IR's *content*, not the iteration order any one build happened to
// produce; a map-order leak in loader/analyze/signatures therefore shows up as a
// difference between two builds of the same input rather than only as a confusing
// downstream golden diff.
func canonicalIR(t *testing.T, dir string) string {
	t.Helper()
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var lines []string

	// Packages: import path plus the sorted basenames of their compiled files.
	for _, p := range res.Service.Packages {
		files := make([]string, 0, len(p.CompiledGoFiles))
		for _, f := range p.CompiledGoFiles {
			files = append(files, filepath.Base(f))
		}
		sort.Strings(files)
		lines = append(lines, "pkg "+p.PkgPath+" ["+strings.Join(files, ",")+"]")
	}

	// Roots: the (FQN, kind, name) identity plus the rendered signature, so an
	// order leak in either roots discovery or signature rendering is caught here.
	for _, r := range res.Roots.Roots {
		lines = append(lines, "root "+r.FQN()+" "+string(r.Kind)+" "+r.Name+" :: "+signatures.Of(r.Func))
	}

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestFrontEndRunTwiceDeterministic builds the front-end IR twice from the same
// input and asserts byte-equality. The downstream graphio goldens already guard
// run-twice stability of the *output*; this guards the *inputs* to that output at
// their source, so an order leak fails with a precise front-end signal.
func TestFrontEndRunTwiceDeterministic(t *testing.T) {
	dir := statictest.FixtureDir()

	first := canonicalIR(t, dir)
	if strings.TrimSpace(first) == "" {
		t.Fatal("canonical IR is empty; the fixture produced no packages or roots")
	}

	for i := 0; i < 3; i++ {
		again := canonicalIR(t, dir)
		if again != first {
			t.Fatalf("front-end IR not deterministic on run %d:\n--- first ---\n%s\n--- again ---\n%s",
				i+1, first, again)
		}
	}
}
