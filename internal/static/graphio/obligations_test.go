package graphio

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
)

// Obligation sites are the first source positions emitted into graph.json; they
// must be module-relative so the output is byte-identical regardless of where
// flowmap was invoked from (path-obligations plan, outcome O2).
func TestObligationsPathInvariant(t *testing.T) {
	rel := filepath.Join("..", "..", "..", "testdata", "groundwork", "obligsvc")
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	marshal := func(dir string) []byte {
		res, err := analyze.Analyze(dir)
		if err != nil {
			t.Fatalf("analyze %s: %v", dir, err)
		}
		g, err := Build(res, "")
		if err != nil {
			t.Fatalf("build %s: %v", dir, err)
		}
		b, err := g.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := marshal(rel), marshal(abs)
	if !bytes.Equal(a, b) {
		t.Fatal("graph.json differs between relative and absolute invocation paths")
	}
	if !bytes.Contains(a, []byte(`"obligations"`)) {
		t.Fatal("obligsvc graph carries no obligations section")
	}
}

// RF-5: obligations are a whole-service disclosure; an entry-scoped view must
// not carry the section (UNMATCHED there would be a scoping artifact).
func TestEntryScopedBuildOmitsObligations(t *testing.T) {
	res, err := analyze.Analyze(filepath.Join("..", "..", "..", "testdata", "groundwork", "obligsvc"))
	if err != nil {
		t.Fatal(err)
	}
	entry := ""
	for _, r := range res.Roots.Roots {
		if r.Name != "" {
			entry = r.Name
			break
		}
	}
	if entry == "" {
		t.Fatal("obligsvc has no named entrypoint to scope to")
	}
	g, err := Build(res, entry)
	if err != nil {
		t.Fatal(err)
	}
	if g.Obligations != nil {
		t.Fatalf("entry-scoped build carries %d obligations; the section is full-graph-only", len(g.Obligations))
	}
}
