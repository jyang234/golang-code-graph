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
	return taint.Analyze(analyzeFixture(t).Program, cfg)
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

// The report must be byte-identical across repeated runs: Go randomizes map-iteration
// order per range, so an unsorted EscapeSites/Flows collection would surface here.
func TestDeterministic(t *testing.T) {
	cfg := taint.Config{
		SourceFuncs: []taint.FuncSpec{src("sourceMap"), src("sourceDirect")},
		Sinks:       []taint.FuncSpec{sink("sinkMap"), sink("sinkDirect")},
	}
	prog := analyzeFixture(t).Program
	want, err := json.Marshal(taint.Analyze(prog, cfg))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		got, err := json.Marshal(taint.Analyze(prog, cfg))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("taint report not deterministic across runs:\n want %s\n got  %s", want, got)
		}
	}
}
