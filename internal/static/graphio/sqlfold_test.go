package graphio_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/static/sqlfold"
)

func builderFixture(t *testing.T) *analyze.Result {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "sqlbuildersvc")
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze sqlbuildersvc: %v", err)
	}
	return res
}

// dbLabels returns the set of "db …" boundary labels and whether each carries the
// fold's provenance tag.
func dbLabels(g *graphio.Graph) map[string]string {
	out := map[string]string{}
	for _, e := range g.Edges {
		if label, ok := strings.CutPrefix(e.To, "boundary:db "); ok {
			out[label] = e.Via
		}
	}
	return out
}

// Without the opt-in, the builder SQL stays opaque (method-name fallback) and no
// edge carries a fold tag — the default build is unchanged (D2).
func TestSQLFoldOffByDefault(t *testing.T) {
	g, err := graphio.Build(builderFixture(t), "")
	if err != nil {
		t.Fatal(err)
	}
	labels := dbLabels(g)
	if _, ok := labels["QueryRowContext"]; !ok {
		t.Errorf("without --reclaim-sql the builder reads should be opaque method names; got %v", labels)
	}
	for label, via := range labels {
		if via != "" {
			t.Errorf("default build must carry no fold provenance; %q has via=%q", label, via)
		}
	}
}

// With --reclaim-sql the fold recovers each verb (and constant table), tags the
// edge, and leaves the genuinely-dynamic verb opaque — the whole trichotomy, end
// to end through the labeler.
func TestSQLFoldRecoversVerbsWithProvenance(t *testing.T) {
	g, err := graphio.Build(builderFixture(t), "", graphio.WithSQLFold())
	if err != nil {
		t.Fatal(err)
	}
	labels := dbLabels(g)

	// Recovered labels, each provenance-tagged. The finite-constant table set fans
	// out into one edge per resolved table (DELETE publishers / DELETE subscribers),
	// while the parameter-table DELETE stays unnamed.
	for _, want := range []string{
		"SELECT messages", "INSERT messages", "DELETE", "UPDATE accounts",
		"DELETE publishers", "DELETE subscribers",
	} {
		via, ok := labels[want]
		if !ok {
			t.Errorf("want recovered label %q; got %v", want, labels)
			continue
		}
		if via != sqlfold.Via {
			t.Errorf("recovered label %q must carry via=%q, got %q", want, sqlfold.Via, via)
		}
	}
	// The dynamic-verb site must stay opaque and untagged (fail closed).
	if via, ok := labels["ExecContext"]; !ok {
		t.Errorf("the dynamic-verb site should stay an opaque method name; got %v", labels)
	} else if via != "" {
		t.Errorf("an abstained site must not be tagged; got via=%q", via)
	}
}

// Determinism: the folded build is a pure function of the SSA — byte-identical
// across repeated builds (CLAUDE.md determinism-test rule for a new label path).
func TestSQLFoldDeterministic(t *testing.T) {
	res := builderFixture(t)
	first, err := graphio.Build(res, "", graphio.WithSQLFold())
	if err != nil {
		t.Fatal(err)
	}
	a, err := first.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		g, err := graphio.Build(res, "", graphio.WithSQLFold())
		if err != nil {
			t.Fatal(err)
		}
		b, err := g.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("folded build is not byte-identical across runs (iteration %d)", i)
		}
	}
}
