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

// TestDetectEmitsNoDeclaredKind guards the producer boundary the ratchet/frontier
// skips depend on: Detect must NEVER emit a Ratified (config-declared) kind, or an
// unratified seam would silently bypass the drift ratchet (fail-open). It checks
// Detect's output over the standard analysis fixture, so it catches the regression
// for every detector the fixture EXERCISES — verified non-vacuous by mutating the
// NonConstantBoundaryArg branch (the notify publish), which this then flags.
//
// It is NOT exhaustive: detector branches the fixture does not reach (reflect,
// HighFanOut, unsafe/cgo/linkname have no trigger in loansvc) are not covered
// behaviorally. The structural half of the guarantee is the single declaredKinds()
// source Ratified() derives from — a new declared kind is classified there once, and
// the consumer-side skips (review/frontier) pick it up without a divergent literal.
// (The fix guard ensures Detect's output is non-empty so this cannot pass vacuously.)
func TestDetectEmitsNoDeclaredKind(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	got := blindspots.Detect(res, features.NewHintSet(res.Config))
	if len(got) == 0 {
		t.Fatal("Detect produced no blind spots over the fixture; the disjointness check would pass vacuously")
	}
	for _, b := range got {
		if b.Kind.Ratified() {
			t.Errorf("Detect emitted a Ratified (config-declared-only) kind %q at %q; the ratchet/frontier skip would treat auto-detected drift as a reviewed declaration", b.Kind, b.Site)
		}
	}
}

func TestKindBoundaryClassification(t *testing.T) {
	gated := []blindspots.Kind{blindspots.NonConstantBoundaryArg, blindspots.UnresolvedDispatch}
	nonGated := []blindspots.Kind{blindspots.Reflect, blindspots.HighFanOut, blindspots.Unsafe, blindspots.Cgo, blindspots.Linkname, blindspots.UnresolvedCall, blindspots.ConcurrentDispatch, blindspots.ExternalBoundaryCall}
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

// TestDetectUnresolvedFuncValueCall pins the zero-resolution mirror of HighFanOut: a
// func-value call the algorithm binds to NO callee must surface as a graph-completeness
// UnresolvedCall, while a func-value call that DOES resolve (via the reachable
// address-taken set) must NOT. The unresolved site (h.cb of type func(string), a field
// never assigned a first-party func) is exactly the seam init()-rooting can never
// recover — so it pins this detector independently of the roots fix.
func TestDetectUnresolvedFuncValueCall(t *testing.T) {
	const src = `package main

// step's address is taken (passed to runResolved below), so RTA resolves the
// func(int) call through it — runResolved.f() must NOT be flagged.
func step(int) {}

func runResolved(f func(int)) { f(2) }

// handler.cb is a func(string) field; no func(string) has its address taken
// anywhere reachable, so cb("x") binds to no callee — the seam B discloses.
type handler struct{ cb func(string) }

func callUnresolved(h handler) { h.cb("x") }

func main() {
	runResolved(step)
	callUnresolved(handler{})
}
`
	res := analyzeModule(t, map[string]string{
		"go.mod":  "module example.com/m\n\ngo 1.24\n",
		"main.go": src,
	})
	bs := blindspots.Detect(res, features.NewHintSet(res.Config))

	var sites []string
	for _, b := range bs {
		if b.Kind == blindspots.UnresolvedCall {
			sites = append(sites, b.Site)
			if b.Kind.Boundary() {
				t.Errorf("UnresolvedCall leaked into the gated boundary subset at %q", b.Site)
			}
		}
	}
	if len(sites) != 1 || !strings.HasSuffix(sites[0], ".callUnresolved") {
		t.Fatalf("UnresolvedCall sites = %v, want exactly [m.callUnresolved] (runResolved must resolve, not flag)", sites)
	}
	// It rides the non-gated graph subset, never the boundary contract.
	for _, b := range blindspots.Boundary(bs) {
		if b.Kind == blindspots.UnresolvedCall {
			t.Errorf("UnresolvedCall must not gate; found in boundary subset: %+v", b)
		}
	}
}

// TestDetectUnresolvedCallDeterministic is the determinism test the UnresolvedCall
// disclosure path ships with (CLAUDE.md: "New ordering or canonicalization paths ship
// with a determinism test"). TestDetectDeterministic runs on loansvc, which emits ZERO
// UnresolvedCall, so it cannot catch a regression in this path. This module emits
// SEVERAL (multiple unserved func-value calls across functions), so any map-iteration
// or arrival-order leak in unresolvedFuncValueCalls / the surrounding Detect loop would
// surface as a run-to-run difference. The walk is over fn.Blocks/b.Instrs (intrinsic
// SSA order) and the manifest is SortBlindSpots'd, so the output must be byte-stable.
func TestDetectUnresolvedCallDeterministic(t *testing.T) {
	const src = `package main

type handler struct{ a, b func(string) }

func one(h handler)   { h.a("1") }
func two(h handler)   { h.b("2") }
func three(h handler) { h.a("3"); h.b("4") }

func main() {
	one(handler{})
	two(handler{})
	three(handler{})
}
`
	res := analyzeModule(t, map[string]string{
		"go.mod":  "module example.com/m\n\ngo 1.24\n",
		"main.go": src,
	})
	hints := features.NewHintSet(res.Config)

	first := blindspots.Detect(res, hints)
	var unresolved int
	for _, b := range first {
		if b.Kind == blindspots.UnresolvedCall {
			unresolved++
		}
	}
	if unresolved == 0 {
		t.Fatal("module emitted no UnresolvedCall; the determinism check would be vacuous")
	}
	// Repeat: the manifest must be identical every run (order and content).
	for i := 0; i < 8; i++ {
		if got := blindspots.Detect(res, hints); !reflect.DeepEqual(first, got) {
			t.Fatalf("UnresolvedCall detection not deterministic on run %d:\n%+v\n%+v", i, first, got)
		}
	}
}

// TestDetectConcurrentDispatch pins the shape recovery: a zero-resolution func
// value dispatched by `go` surfaces as ConcurrentDispatch (the machine states the
// hidden body is asynchronous), while the same call made synchronously stays an
// UnresolvedCall — proving the SSA instruction type, not a guess, drives the
// split. Both ride the non-gated graph subset, and the manifest stays byte-stable
// across runs (the determinism test this new ordering path ships with).
func TestDetectConcurrentDispatch(t *testing.T) {
	const src = `package main

// h.cb is a func(string) field never assigned a first-party func, so cb("x")
// binds to no callee. async launches it with ` + "`go`" + ` (an *ssa.Go); sync calls
// it directly (an *ssa.Call). Same unresolved seam, different SSA shape.
type handler struct{ cb func(string) }

func async(h handler) { go h.cb("x") }
func sync(h handler)  { h.cb("y") }

func main() {
	async(handler{})
	sync(handler{})
}
`
	res := analyzeModule(t, map[string]string{
		"go.mod":  "module example.com/m\n\ngo 1.24\n",
		"main.go": src,
	})
	hints := features.NewHintSet(res.Config)
	bs := blindspots.Detect(res, hints)

	var concurrent, unresolved []string
	for _, b := range bs {
		switch b.Kind {
		case blindspots.ConcurrentDispatch:
			concurrent = append(concurrent, b.Site)
			if !strings.Contains(b.Detail, "goroutine") {
				t.Errorf("ConcurrentDispatch detail should name the goroutine shape, got %q", b.Detail)
			}
		case blindspots.UnresolvedCall:
			unresolved = append(unresolved, b.Site)
		}
	}
	if len(concurrent) != 1 || !strings.HasSuffix(concurrent[0], ".async") {
		t.Fatalf("ConcurrentDispatch sites = %v, want exactly [m.async]", concurrent)
	}
	if len(unresolved) != 1 || !strings.HasSuffix(unresolved[0], ".sync") {
		t.Fatalf("UnresolvedCall sites = %v, want exactly [m.sync] (the `go` site must not be flagged plain)", unresolved)
	}
	// Neither shape gates: both are graph-completeness disclosures.
	for _, b := range blindspots.Boundary(bs) {
		if b.Kind == blindspots.ConcurrentDispatch {
			t.Errorf("ConcurrentDispatch must not gate; found in boundary subset: %+v", b)
		}
	}
	// Byte-stable across runs.
	for i := 0; i < 8; i++ {
		if got := blindspots.Detect(res, hints); !reflect.DeepEqual(bs, got) {
			t.Fatalf("ConcurrentDispatch detection not deterministic on run %d", i)
		}
	}
}

// TestDetectExternalBoundaryCall pins the unclassified-external-dependency
// surface over the loansvc fixture: a first-party handoff into a third-party
// (non-stdlib) package that is not a classified boundary effect surfaces as an
// ExternalBoundaryCall naming the package. loansvc calls golang.org/x/sync/errgroup
// directly (a genuine concurrency dependency) — that must surface. It also
// instruments with OpenTelemetry on nearly every function; those span/attribute
// calls must NOT surface (the isInstrumentation exclusion), or the disclosure
// would drown in per-span noise. And it must never gate.
func TestDetectExternalBoundaryCall(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	bs := blindspots.Detect(res, features.NewHintSet(res.Config))

	var sawErrgroup, sawOTel bool
	for _, b := range bs {
		if b.Kind != blindspots.ExternalBoundaryCall {
			continue
		}
		switch {
		case strings.Contains(b.Detail, "golang.org/x/sync/errgroup"):
			sawErrgroup = true
		case strings.Contains(b.Detail, "go.opentelemetry.io/"):
			sawOTel = true
		}
		if b.Kind.Boundary() {
			t.Errorf("ExternalBoundaryCall leaked into the gated boundary subset at %q", b.Site)
		}
	}
	if !sawErrgroup {
		t.Errorf("expected an ExternalBoundaryCall for golang.org/x/sync/errgroup; manifest=%+v", bs)
	}
	if sawOTel {
		t.Errorf("OpenTelemetry instrumentation must be excluded from ExternalBoundaryCall (isInstrumentation), but one surfaced")
	}
	// A classified third-party boundary (database/sql is stdlib; the bus/HTTP hints
	// cover the rest) must not double-emit as EBC: stdlib is excluded by rule, and
	// any hinted callee is excluded. The non-gated subset carries every EBC.
	for _, b := range blindspots.Boundary(bs) {
		if b.Kind == blindspots.ExternalBoundaryCall {
			t.Errorf("ExternalBoundaryCall must not gate; found in boundary subset: %+v", b)
		}
	}
}

// TestExternalBoundaryExemptSuppresses pins the suppression mechanism: a
// static.externalBoundaryExempt prefix removes a package's ExternalBoundaryCall
// while leaving the rest of the manifest intact. loansvc's one genuine EBC is
// golang.org/x/sync/errgroup; exempting that prefix must drop exactly it and
// nothing else (the disclosure narrows, it does not vanish wholesale).
func TestExternalBoundaryExemptSuppresses(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}

	count := func(hints *features.HintSet) (ebc int, other int) {
		for _, b := range blindspots.Detect(res, hints) {
			if b.Kind == blindspots.ExternalBoundaryCall {
				ebc++
			} else {
				other++
			}
		}
		return
	}

	baseEBC, baseOther := count(features.NewHintSet(res.Config))
	if baseEBC == 0 {
		t.Fatal("loansvc should emit at least one ExternalBoundaryCall; the suppression check would be vacuous")
	}

	// Same classification config as the base, with only the exempt prefix added, so
	// the exempt list is the sole variable.
	cfg := *res.Config
	cfg.Static.ExternalBoundaryExempt = append([]string{"golang.org/x/sync"}, cfg.Static.ExternalBoundaryExempt...)
	exEBC, exOther := count(features.NewHintSet(&cfg))

	if exEBC >= baseEBC {
		t.Errorf("exempting golang.org/x/sync should reduce ExternalBoundaryCall count: base=%d exempt=%d", baseEBC, exEBC)
	}
	// Suppression is surgical: it touches only the EBC channel, never other kinds.
	if exOther != baseOther {
		t.Errorf("exempt list must not change non-EBC disclosures: base=%d exempt=%d", baseOther, exOther)
	}
	for _, b := range blindspots.Detect(res, features.NewHintSet(&cfg)) {
		if b.Kind == blindspots.ExternalBoundaryCall && strings.Contains(b.Detail, "golang.org/x/sync") {
			t.Errorf("golang.org/x/sync EBC should be suppressed, still present: %+v", b)
		}
	}
}

// TestExternalBoundaryCallSeverityTier pins the §21.A signal/noise tier: an EBC is
// tagged effect-bearing by DEFAULT (disclose, don't pre-judge an unrecognized
// dependency), and trivial only when its package is on the known-benign set. loansvc's
// errgroup handoff ORCHESTRATES first-party closures that reach the gateway and bus, so
// it is effect-bearing; declaring its prefix trivial via config re-tags it WITHOUT
// changing the count (the tier is disclosure-only — it reprioritizes, it never
// suppresses, unlike externalBoundaryExempt).
func TestExternalBoundaryCallSeverityTier(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}

	ebcs := func(hints *features.HintSet) []blindspots.BlindSpot {
		var out []blindspots.BlindSpot
		for _, b := range blindspots.Detect(res, hints) {
			if b.Kind == blindspots.ExternalBoundaryCall {
				out = append(out, b)
			}
		}
		return out
	}

	// Default: errgroup is effect-bearing (not on the benign built-in set), and every
	// EBC carries a non-empty tier (no spot is left unclassified by Detect).
	base := ebcs(features.NewHintSet(res.Config))
	if len(base) == 0 {
		t.Fatal("loansvc should emit at least one ExternalBoundaryCall")
	}
	var sawErrgroup bool
	for _, b := range base {
		if b.Severity != blindspots.SeverityEffectBearing && b.Severity != blindspots.SeverityTrivial {
			t.Errorf("EBC must carry a tier, got %q at %s", b.Severity, b.Site)
		}
		if b.Package == "golang.org/x/sync/errgroup" {
			sawErrgroup = true
			if b.Severity != blindspots.SeverityEffectBearing {
				t.Errorf("errgroup orchestrates effect-bearing closures; want effect-bearing, got %q", b.Severity)
			}
		}
	}
	if !sawErrgroup {
		t.Fatalf("expected the errgroup EBC; manifest=%+v", base)
	}

	// Declaring the prefix trivial re-tags it without dropping it: same EBC count, but
	// errgroup is now trivial — the disclosure-only tier, distinct from exempt's drop.
	cfg := *res.Config
	cfg.Static.ExternalBoundaryTrivial = append([]string{"golang.org/x/sync"}, cfg.Static.ExternalBoundaryTrivial...)
	tagged := ebcs(features.NewHintSet(&cfg))
	if len(tagged) != len(base) {
		t.Errorf("trivial tier must not change the EBC count (disclosure-only): base=%d tagged=%d", len(base), len(tagged))
	}
	for _, b := range tagged {
		if b.Package == "golang.org/x/sync/errgroup" && b.Severity != blindspots.SeverityTrivial {
			t.Errorf("config trivial prefix should tag errgroup trivial, got %q", b.Severity)
		}
	}
}

// TestExternalPackageStructured pins §21.B: the target package rides as STRUCTURED
// data on the blind spot (Package), set for every ExternalBoundaryCall and empty for
// every other kind, and the same path also appears in the human Detail prose — so a
// renderer labels the boundary node from the field, never by parsing the prose.
func TestExternalPackageStructured(t *testing.T) {
	res, err := statictest.Analyze()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blindspots.Detect(res, features.NewHintSet(res.Config)) {
		if b.Kind != blindspots.ExternalBoundaryCall {
			if b.Package != "" {
				t.Errorf("%s blind spot must carry no Package, got %q", b.Kind, b.Package)
			}
			continue
		}
		if b.Package == "" {
			t.Errorf("ExternalBoundaryCall at %s must carry a Package", b.Site)
		}
		if !strings.Contains(b.Detail, b.Package) {
			t.Errorf("Detail %q must name the structured Package %q", b.Detail, b.Package)
		}
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

// TestHighFanOutThresholdConfigurable raises the threshold above the synthetic
// module's fan-out and confirms the disclosure is suppressed — proving the knob.
func TestHighFanOutThresholdConfigurable(t *testing.T) {
	res := analyzeModule(t, map[string]string{
		"go.mod":        "module example.com/m\n\ngo 1.24\n",
		".flowmap.yaml": "static:\n  highFanOutThreshold: 20\n",
		"main.go":       highFanOutMain(10), // 10 callees < 20 threshold
	})
	for _, b := range blindspots.Detect(res, features.NewHintSet(res.Config)) {
		if b.Kind == blindspots.HighFanOut {
			t.Errorf("fan-out of 10 should be under the configured threshold of 20, got %+v", b)
		}
	}
}
