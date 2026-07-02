package taint_test

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/taint"
)

const pkg = "example.com/taintsvc"

func analyzeFixture(t *testing.T) *analyze.Result {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "taintsvc")
	res, err := analyze.Analyze(dir)
	if err != nil {
		t.Fatalf("analyze taintsvc: %v", err)
	}
	return res
}

func run(t *testing.T, cfg taint.Config) taint.Report {
	t.Helper()
	res := analyzeFixture(t)
	return taint.Analyze(res.Program, res.Graph, cfg)
}

func src(name string) taint.FuncSpec  { return taint.FuncSpec{Pkg: pkg, Name: name} }
func sink(name string) taint.FuncSpec { return taint.FuncSpec{Pkg: pkg, Name: name} }

// The four FLOW cases — each must be a could-flow candidate, never a (false) NO-FLOW.
func TestFlows(t *testing.T) {
	cases := []struct{ name, source, sink string }{
		{"direct", "sourceDirect", "sinkDirect"},
		{"interproc-arg", "sourceRelay", "sinkRelay"},
		{"interproc-return", "sourceReturn", "sinkReturn"},
		{"struct-field-round-trip", "sourceField", "sinkField"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := run(t, taint.Config{
				SourceFuncs: []taint.FuncSpec{src(c.source)},
				Sinks:       []taint.FuncSpec{sink(c.sink)},
			})
			if r.Verdict != taint.Flow {
				t.Fatalf("%s: verdict = %s, want FLOW (escaped=%v flows=%v)", c.name, r.Verdict, r.Escaped, r.Flows)
			}
			if len(r.Flows) == 0 {
				t.Errorf("%s: FLOW verdict but no findings", c.name)
			}
		})
	}
}

// A declared sensitive FIELD read that reaches a sink is a FLOW.
func TestFieldReadSource(t *testing.T) {
	r := run(t, taint.Config{
		SourceFields: []taint.FieldSpec{{Pkg: pkg, Type: "Recipient", Field: "Secret"}},
		Sinks:        []taint.FuncSpec{sink("sinkFieldRead")},
	})
	if r.Verdict != taint.Flow {
		t.Fatalf("field-read source: verdict = %s, want FLOW", r.Verdict)
	}
	if r.Sources == 0 {
		t.Errorf("field source was not matched/seeded")
	}
}

// The map case is the SOUNDNESS guard: taint into a map is the frontier, so the
// verdict must be ABSTAIN — NEVER a (false) NO-FLOW.
func TestMapEscapesToAbstain(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceMap")},
		Sinks:       []taint.FuncSpec{sink("sinkMap")},
	})
	if r.Verdict != taint.Abstain {
		t.Fatalf("map case: verdict = %s, want ABSTAIN (a false NO-FLOW here is the worst outcome)", r.Verdict)
	}
	if !r.Escaped || len(r.EscapeSites) == 0 {
		t.Errorf("map case must disclose an escape site; escaped=%v sites=%v", r.Escaped, r.EscapeSites)
	}
	if len(r.Flows) != 0 {
		t.Errorf("map case must not report a (tracked) flow; got %v", r.Flows)
	}
}

// The clean case is the only PROVEN no-flow: a source whose complete forward cone
// never reaches the sink and never escaped.
func TestCleanIsNoFlow(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceClean")},
		Sinks:       []taint.FuncSpec{sink("sinkClean")},
	})
	if r.Verdict != taint.NoFlow {
		t.Fatalf("clean case: verdict = %s, want NO-FLOW (escaped=%v flows=%v sites=%v)", r.Verdict, r.Escaped, r.Flows, r.EscapeSites)
	}
}

// A sensitive field read located only inside a pointer-receiver (*T) method must
// still be seeded: MethodSet(T) omits *T methods, so firstPartyFuncs must walk both
// method sets. Before the fix this returned a false NO-FLOW (sources seeded: 0).
func TestPointerReceiverFieldSourceIsSeeded(t *testing.T) {
	r := run(t, taint.Config{
		SourceFields: []taint.FieldSpec{{Pkg: pkg, Type: "PtrCarrier", Field: "Token"}},
		Sinks:        []taint.FuncSpec{sink("sinkPtr")},
	})
	if r.Verdict != taint.Flow {
		t.Fatalf("pointer-receiver field source: verdict = %s, want FLOW (escaped=%v sources=%d) — a *T-method read must be seeded, not silently proven safe", r.Verdict, r.Escaped, r.Sources)
	}
	if r.Sources == 0 {
		t.Error("the *T-method field read was never seeded (MethodSet(T) omits pointer methods)")
	}
}

// TestInterfaceReturnTaintNotProvenClean is the C-3 regression: taint returned
// through an interface (invoke) method must not silently die. The call graph
// resolves the invoke to the concrete taint-returning method, so the invoke
// result carries the taint to the sink → FLOW, never a (false) NO-FLOW.
func TestInterfaceReturnTaintNotProvenClean(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceIface")},
		Sinks:       []taint.FuncSpec{sink("sinkIface")},
	})
	if r.Verdict == taint.NoFlow {
		t.Fatalf("interface-return taint: verdict = NO-FLOW — the invoke result was silently proven clean (C-3)")
	}
	if r.Verdict != taint.Flow {
		t.Errorf("interface-return taint: verdict = %s, want FLOW (escaped=%v flows=%v)", r.Verdict, r.Escaped, r.Flows)
	}
}

// TestInterfaceSourceIsSeeded is the C-4 regression: a declared source method
// invoked via an interface must still be seeded, else Sources==0 → false NO-FLOW.
func TestInterfaceSourceIsSeeded(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("Provide")},
		Sinks:       []taint.FuncSpec{sink("sinkProvide")},
	})
	if r.Verdict == taint.NoFlow {
		t.Fatalf("interface source: verdict = NO-FLOW — the source invoked via interface was never seeded (C-4)")
	}
	if r.Sources == 0 {
		t.Errorf("interface source: Sources==0 — the invoke-site source was not seeded")
	}
	if r.Verdict != taint.Flow {
		t.Errorf("interface source: verdict = %s, want FLOW", r.Verdict)
	}
}

// TestStructCarriedFieldEscapes is the C-5 regression: a struct value carrying a
// tainted field, handed WHOLE to an unmodeled callee (fmt.Println), must escape —
// tainting the (type,field) key alone left the whole-struct hand-off invisible, a
// false NO-FLOW. The whole-struct use has no declared sink, so the sound verdict
// is ABSTAIN.
func TestStructCarriedFieldEscapes(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceCarry")},
		Sinks:       []taint.FuncSpec{sink("sinkClean")}, // an unrelated declared sink
	})
	if r.Verdict == taint.NoFlow {
		t.Fatalf("struct-carried field: verdict = NO-FLOW — the whole-struct hand-off was silently proven clean (C-5)")
	}
	if r.Verdict != taint.Abstain || !r.Escaped {
		t.Errorf("struct-carried field: verdict = %s escaped=%v, want ABSTAIN with an escape site", r.Verdict, r.Escaped)
	}
}

// TestInterfaceArgReachesParam is the C-3 arg→param regression: a tainted argument
// passed into an interface method must reach the parameter inside the concrete impl
// (the invoke receiver-offset bug tainted the receiver and missed the real param).
func TestInterfaceArgReachesParam(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceIfaceArg")},
		Sinks:       []taint.FuncSpec{sink("sinkIfaceArg")},
	})
	if r.Verdict != taint.Flow {
		t.Fatalf("interface arg→param: verdict = %s (escaped=%v flows=%v), want FLOW", r.Verdict, r.Escaped, r.Flows)
	}
}

// TestFuncValueReturnFlow is the C-3 func-value regression: return-flow through a
// plain func value (StaticCallee nil, not an invoke) must not be dropped.
func TestFuncValueReturnFlow(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceFuncVal")},
		Sinks:       []taint.FuncSpec{sink("sinkFuncVal")},
	})
	if r.Verdict == taint.NoFlow {
		t.Fatalf("func-value return-flow: verdict = NO-FLOW — a taint-returning func value was proven clean")
	}
	if r.Verdict != taint.Flow {
		t.Errorf("func-value return-flow: verdict = %s, want FLOW", r.Verdict)
	}
}

// TestGenericSourceIsSeeded is the C-4 regression: a declared source that is a
// first-party generic function must be seeded through its nil-Pkg instance callee.
func TestGenericSourceIsSeeded(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("GenericSource")},
		Sinks:       []taint.FuncSpec{sink("sinkGeneric")},
	})
	if r.Sources == 0 {
		t.Errorf("generic source: Sources==0 — the instance callee was not matched/seeded")
	}
	if r.Verdict != taint.Flow {
		t.Errorf("generic source: verdict = %s, want FLOW", r.Verdict)
	}
}

// Indexing a tainted slice is an unmodeled frontier, so the propagate switch's
// default-escape backstop must fire: ABSTAIN, never a (false) NO-FLOW. This is the
// soundness regression guard for the missing-default bug.
func TestSliceIndexEscapesToAbstain(t *testing.T) {
	r := run(t, taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceSlice")},
		Sinks:       []taint.FuncSpec{sink("sinkSlice")},
	})
	if r.Verdict != taint.Abstain {
		t.Fatalf("slice-index case: verdict = %s, want ABSTAIN (a false NO-FLOW here is the missing-default soundness bug)", r.Verdict)
	}
	if !r.Escaped {
		t.Errorf("slice-index case must set escaped")
	}
}

// AnalyzeBySource must DECOMPOSE without changing the aggregate: the per-source runs
// union back to exactly Analyze(all sources). This is the soundness contract of the
// decomposition (taint is a monotone per-value union) — decomposing must not invent a
// flow/escape the aggregate lacks, nor drop one it has. The fixture mixes a FLOW
// source (sourceDirect), an ABSTAIN source (sourceMap) and a NO-FLOW source
// (sourceClean): the aggregate is FLOW and would MASK the other two, which is exactly
// what per-source reporting recovers.
func TestAnalyzeBySource_Decomposes(t *testing.T) {
	cfg := taint.Config{
		SourceFuncs:  []taint.FuncSpec{src("sourceDirect"), src("sourceMap"), src("sourceClean")},
		SourceFields: []taint.FieldSpec{{Pkg: pkg, Type: "Recipient", Field: "Secret"}},
		Sinks:        []taint.FuncSpec{sink("sinkDirect"), sink("sinkMap"), sink("sinkClean"), sink("sinkFieldRead")},
	}
	fx := analyzeFixture(t)
	prog, cg := fx.Program, fx.Graph
	agg := taint.Analyze(prog, cg, cfg)
	per := taint.AnalyzeBySource(prog, cg, cfg)

	// One report per declared source, in canonical (sorted) Source order.
	if len(per) != len(cfg.SourceFuncs)+len(cfg.SourceFields) {
		t.Fatalf("want %d per-source reports, got %d", len(cfg.SourceFuncs)+len(cfg.SourceFields), len(per))
	}
	for i := 1; i < len(per); i++ {
		if per[i-1].Source >= per[i].Source {
			t.Errorf("per-source reports not in canonical order: %q before %q", per[i-1].Source, per[i].Source)
		}
	}

	// Union identity: the per-source flows / escape sites / seed counts / escaped flag
	// must reconstruct the aggregate exactly — nothing invented, nothing dropped.
	gotFlows := map[taint.Finding]bool{}
	gotSites := map[string]bool{}
	escaped, seeds := false, 0
	for _, sr := range per {
		for _, f := range sr.Report.Flows {
			gotFlows[f] = true
		}
		for _, s := range sr.Report.EscapeSites {
			gotSites[s] = true
		}
		escaped = escaped || sr.Report.Escaped
		seeds += sr.Report.Sources
	}
	if escaped != agg.Escaped {
		t.Errorf("union escaped = %v, aggregate = %v", escaped, agg.Escaped)
	}
	if seeds != agg.Sources {
		t.Errorf("sum of per-source seeds = %d, aggregate = %d", seeds, agg.Sources)
	}
	if len(gotFlows) != len(agg.Flows) {
		t.Errorf("union flows = %d, aggregate = %d", len(gotFlows), len(agg.Flows))
	}
	for _, f := range agg.Flows {
		if !gotFlows[f] {
			t.Errorf("aggregate flow %+v not present in any per-source report (a masked/dropped flow)", f)
		}
	}
	for _, s := range agg.EscapeSites {
		if !gotSites[s] {
			t.Errorf("aggregate escape site %q not present in any per-source report", s)
		}
	}

	// The whole point: the aggregate is a single FLOW that masks the rest; per-source
	// must surface at least one non-FLOW verdict so the decomposition is informative.
	if agg.Verdict != taint.Flow {
		t.Fatalf("fixture precondition: aggregate should be FLOW, got %s", agg.Verdict)
	}
	sawNonFlow := false
	for _, sr := range per {
		if sr.Report.Verdict != taint.Flow {
			sawNonFlow = true
		}
	}
	if !sawNonFlow {
		t.Error("per-source decomposition surfaced no non-FLOW verdict — it would add nothing over the aggregate")
	}
}

// decompCfg is the multi-source × multi-sink config the decomposition tests share.
func decompCfg() taint.Config {
	return taint.Config{
		SourceFuncs:  []taint.FuncSpec{src("sourceDirect"), src("sourceMap"), src("sourceClean")},
		SourceFields: []taint.FieldSpec{{Pkg: pkg, Type: "Recipient", Field: "Secret"}},
		Sinks:        []taint.FuncSpec{sink("sinkDirect"), sink("sinkMap"), sink("sinkClean"), sink("sinkFieldRead")},
	}
}

// combine is how a set of cell verdicts marginalises: FLOW if any flows, else ABSTAIN if
// any cone escaped, else NO-FLOW — the same precedence as the aggregate trichotomy.
func combine(vs []taint.Verdict) taint.Verdict {
	anyAbstain := false
	for _, v := range vs {
		switch v {
		case taint.Flow:
			return taint.Flow
		case taint.Abstain:
			anyAbstain = true
		}
	}
	if anyAbstain {
		return taint.Abstain
	}
	return taint.NoFlow
}

// AnalyzeBySink must PARTITION the aggregate without inventing a verdict: the per-sink
// flows union back to exactly the aggregate's, each flow is filed under its own sink, and
// — the soundness guard — a sink reads NO-FLOW only when the WHOLE forward cone was
// complete (else ABSTAIN, never a false no-flow from a sink-local view).
func TestAnalyzeBySink_Decomposes(t *testing.T) {
	cfg := decompCfg()
	fx := analyzeFixture(t)
	prog, cg := fx.Program, fx.Graph
	agg := taint.Analyze(prog, cg, cfg)
	per := taint.AnalyzeBySink(prog, cg, cfg)

	if len(per) != len(cfg.Sinks) {
		t.Fatalf("want %d per-sink reports, got %d", len(cfg.Sinks), len(per))
	}
	for i := 1; i < len(per); i++ {
		if per[i-1].Sink >= per[i].Sink {
			t.Errorf("per-sink reports not in canonical order: %q before %q", per[i-1].Sink, per[i].Sink)
		}
	}

	gotFlows := map[taint.Finding]bool{}
	for _, sr := range per {
		for _, f := range sr.Report.Flows {
			if f.Sink != sr.Sink {
				t.Errorf("flow %+v filed under the wrong sink %q", f, sr.Sink)
			}
			gotFlows[f] = true
		}
		// The verdict must match the OBSERVABLE facts directly (a specification, not a
		// re-derived verdict helper): a sink with flows is FLOW; with none it is ABSTAIN
		// iff the aggregate cone escaped (the no-false-no-flow soundness guard), else NO-FLOW.
		switch {
		case len(sr.Report.Flows) > 0:
			if sr.Report.Verdict != taint.Flow {
				t.Errorf("sink %q has flows but verdict %s, want FLOW", sr.Sink, sr.Report.Verdict)
			}
		case agg.Escaped:
			if sr.Report.Verdict != taint.Abstain {
				t.Errorf("sink %q no-flow on an escaped cone but verdict %s, want ABSTAIN (a false no-flow otherwise)", sr.Sink, sr.Report.Verdict)
			}
		default:
			if sr.Report.Verdict != taint.NoFlow {
				t.Errorf("sink %q no-flow on a complete cone but verdict %s, want NO-FLOW", sr.Sink, sr.Report.Verdict)
			}
		}
	}
	if len(gotFlows) != len(agg.Flows) {
		t.Errorf("union of per-sink flows = %d, aggregate = %d", len(gotFlows), len(agg.Flows))
	}
	for _, f := range agg.Flows {
		if !gotFlows[f] {
			t.Errorf("aggregate flow %+v not present in any per-sink report", f)
		}
	}
}

// The (source × sink) matrix is the full decomposition; its cells must MARGINALISE back to
// both AnalyzeBySource (collapsing sinks) and AnalyzeBySink (collapsing sources). This is
// the soundness contract: the matrix neither invents a flow the aggregate lacks nor drops
// one it has, in either projection.
func TestAnalyzeBySourceAndSink_Marginalises(t *testing.T) {
	cfg := decompCfg()
	fx := analyzeFixture(t)
	prog, cg := fx.Program, fx.Graph
	matrix := taint.AnalyzeBySourceAndSink(prog, cg, cfg)
	bySource := taint.AnalyzeBySource(prog, cg, cfg)
	bySink := taint.AnalyzeBySink(prog, cg, cfg)

	wantCells := (len(cfg.SourceFuncs) + len(cfg.SourceFields)) * len(cfg.Sinks)
	if len(matrix) != wantCells {
		t.Fatalf("matrix has %d cells, want %d (sources × sinks)", len(matrix), wantCells)
	}
	for i := 1; i < len(matrix); i++ {
		a, b := matrix[i-1], matrix[i]
		if a.Source > b.Source || (a.Source == b.Source && a.Sink >= b.Sink) {
			t.Errorf("matrix not in canonical (Source, Sink) order at %d: %q/%q then %q/%q", i, a.Source, a.Sink, b.Source, b.Sink)
		}
	}

	bySrcV := map[string][]taint.Verdict{}
	bySnkV := map[string][]taint.Verdict{}
	for _, c := range matrix {
		bySrcV[c.Source] = append(bySrcV[c.Source], c.Report.Verdict)
		bySnkV[c.Sink] = append(bySnkV[c.Sink], c.Report.Verdict)
	}
	for _, sr := range bySource {
		if got := combine(bySrcV[sr.Source]); got != sr.Report.Verdict {
			t.Errorf("source %q: matrix marginal %s != by-source %s", sr.Source, got, sr.Report.Verdict)
		}
	}
	for _, sr := range bySink {
		if got := combine(bySnkV[sr.Sink]); got != sr.Report.Verdict {
			t.Errorf("sink %q: matrix marginal %s != by-sink %s", sr.Sink, got, sr.Report.Verdict)
		}
	}
}

// The report must be byte-identical across repeated runs: Go randomizes map-iteration
// order per range, so an unsorted EscapeSites/Flows collection would surface here.
func TestDeterministic(t *testing.T) {
	cfg := taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceMap"), src("sourceDirect")},
		Sinks:       []taint.FuncSpec{sink("sinkMap"), sink("sinkDirect")},
	}
	fx := analyzeFixture(t)
	prog, cg := fx.Program, fx.Graph
	want, err := json.Marshal(taint.Analyze(prog, cg, cfg))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		got, err := json.Marshal(taint.Analyze(prog, cg, cfg))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("taint report not deterministic across runs:\n want %s\n got  %s", want, got)
		}
	}
}

// The per-source decomposition is a new canonical-ordering path (it sorts on the Source
// key), so it ships its own byte-identical-across-runs guard (CLAUDE.md: "New ordering or
// canonicalization paths ship with a determinism test"). Map-iteration randomness in the
// shared prepared indexes or an unsorted per-source list would surface here.
func TestAnalyzeBySourceDeterministic(t *testing.T) {
	cfg := taint.Config{
		SourceFuncs:  []taint.FuncSpec{src("sourceMap"), src("sourceDirect"), src("sourceClean")},
		SourceFields: []taint.FieldSpec{{Pkg: pkg, Type: "Recipient", Field: "Secret"}},
		Sinks:        []taint.FuncSpec{sink("sinkMap"), sink("sinkDirect"), sink("sinkFieldRead")},
	}
	fx := analyzeFixture(t)
	prog, cg := fx.Program, fx.Graph
	want, err := json.Marshal(taint.AnalyzeBySource(prog, cg, cfg))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		got, err := json.Marshal(taint.AnalyzeBySource(prog, cg, cfg))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("per-source decomposition not deterministic across runs:\n want %s\n got  %s", want, got)
		}
	}
}

// The by-sink and (source × sink) matrix are new canonical-ordering paths too — each sorts
// on its intrinsic key — so they ship byte-identical-across-runs guards (CLAUDE.md). The
// matrix order also depends on the per-source iteration, which the sort must canonicalise.
func TestAnalyzeBySinkAndMatrixDeterministic(t *testing.T) {
	cfg := decompCfg()
	fx := analyzeFixture(t)
	prog, cg := fx.Program, fx.Graph
	for _, gen := range []struct {
		name string
		fn   func() any
	}{
		{"by-sink", func() any { return taint.AnalyzeBySink(prog, cg, cfg) }},
		{"matrix", func() any { return taint.AnalyzeBySourceAndSink(prog, cg, cfg) }},
	} {
		want, err := json.Marshal(gen.fn())
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 25; i++ {
			got, err := json.Marshal(gen.fn())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("%s decomposition not deterministic across runs:\n want %s\n got  %s", gen.name, want, got)
			}
		}
	}
}
