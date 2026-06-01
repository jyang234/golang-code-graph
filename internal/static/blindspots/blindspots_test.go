package blindspots_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/statictest"
)

func TestDetectNonConstantPublish(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	bs := blindspots.Detect(res, features.NewHintSet(res.Config))

	var nonConst int
	for _, b := range bs {
		if b.Kind == blindspots.NonConstantBoundaryArg {
			nonConst++
			if !strings.Contains(b.Site, "notify") {
				t.Errorf("unexpected NonConstantBoundaryArg site %q", b.Site)
			}
		}
	}
	// Exactly one: the notify publish. The three constant publishes and the two
	// constant outbound calls must NOT be false positives.
	if nonConst != 1 {
		t.Errorf("NonConstantBoundaryArg count = %d, want 1: %+v", nonConst, bs)
	}
}

func TestDetectDeterministic(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	hints := features.NewHintSet(res.Config)
	a := blindspots.Detect(res, hints)
	b := blindspots.Detect(res, hints)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("blind-spot detection not deterministic:\n%+v\n%+v", a, b)
	}
}

func TestKindBoundaryClassification(t *testing.T) {
	gated := []blindspots.Kind{blindspots.NonConstantBoundaryArg, blindspots.UnresolvedDispatch}
	nonGated := []blindspots.Kind{blindspots.Reflect, blindspots.HighFanOut, blindspots.Unsafe, blindspots.Cgo, blindspots.Linkname}
	for _, k := range gated {
		if !k.Boundary() {
			t.Errorf("%q should be a gated boundary blind spot", k)
		}
	}
	for _, k := range nonGated {
		if k.Boundary() {
			t.Errorf("%q is a graph-completeness disclosure and must not gate", k)
		}
	}
}

// TestDetectGraphCompletenessDisclosures builds a synthetic module exercising the
// non-gated graph-completeness categories: a widely-implemented interface (high
// fan-out), an unsafe import, and a //go:linkname directive. They must surface in
// the graph subset and never leak into the gated boundary subset.
func TestDetectGraphCompletenessDisclosures(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod":           "module example.com/m\n\ngo 1.24\n",
		"main.go":          highFanOutMain(10),
		"danger/danger.go": "package danger\n\nimport _ \"unsafe\"\n\n//go:linkname helper\nfunc helper() {}\n",
	})
	bs := blindspots.Detect(res, features.NewHintSet(res.Config))

	count := map[blindspots.Kind]int{}
	for _, b := range bs {
		count[b.Kind]++
	}
	for _, k := range []blindspots.Kind{blindspots.HighFanOut, blindspots.Unsafe, blindspots.Linkname} {
		if count[k] == 0 {
			t.Errorf("expected a %q disclosure, manifest=%+v", k, bs)
		}
	}

	// None of these gate.
	for _, b := range blindspots.Boundary(bs) {
		if !b.Kind.Boundary() {
			t.Errorf("graph disclosure %q leaked into the gated boundary subset", b.Kind)
		}
	}
	// All of them ride the graph subset.
	if len(blindspots.Graph(bs)) < 3 {
		t.Errorf("graph subset should hold the disclosures, got %+v", blindspots.Graph(bs))
	}
}

// highFanOutMain renders a main package whose interface has n implementations,
// all instantiated, so RTA resolves the interface call to n callees.
func highFanOutMain(n int) string {
	var b strings.Builder
	b.WriteString("package main\n\ntype I interface{ M() }\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "type T%d struct{}\n\nfunc (T%d) M() {}\n\n", i, i)
	}
	b.WriteString("func pick(n int) I {\n\tswitch n {\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\tcase %d:\n\t\treturn T%d{}\n", i, i)
	}
	b.WriteString("\t}\n\treturn nil\n}\n\nfunc main() {\n\tvar x I = pick(0)\n\tx.M()\n}\n")
	return b.String()
}

// analyzeModule writes a temp module and runs the static front half on it.
func analyzeModule(t *testing.T, files map[string]string) *analyze.Result {
	t.Helper()
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return res
}
