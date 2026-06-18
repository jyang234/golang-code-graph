package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/buildinfo"
	"github.com/jyang234/golang-code-graph/internal/ingest"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestLoadIngestConfigReadsServiceDir: ingest's canon config (e.g. the
// messagingShortHexIDs opt-in) is read from --service-dir's .flowmap.yaml; with no
// service dir, defaults apply.
func TestLoadIngestConfigReadsServiceDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".flowmap.yaml"),
		[]byte("service: svc\ncanon:\n  messagingShortHexIDs: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadIngestConfig(dir)
	if err != nil {
		t.Fatalf("loadIngestConfig: %v", err)
	}
	if cfg == nil || !cfg.Canon.MessagingShortHexIDs {
		t.Errorf("opt-in not loaded from service dir: %+v", cfg)
	}
	if cfg, err := loadIngestConfig(""); err != nil || cfg != nil {
		t.Errorf("no service dir should yield nil defaults, got cfg=%v err=%v", cfg, err)
	}
}

// TestParsePermutedAcceptsFlagsEitherSide: the traces positional may appear before
// or after the flags (Go's flag package alone would stop at the first positional and
// silently drop trailing flags).
func TestParsePermutedAcceptsFlagsEitherSide(t *testing.T) {
	for _, args := range [][]string{
		{"traces.json", "--render-dir", "out", "--update"},
		{"--render-dir", "out", "traces.json", "--update"},
		{"--render-dir", "out", "--update", "traces.json"},
	} {
		fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
		renderDir := fs.String("render-dir", "", "")
		update := fs.Bool("update", false, "")
		got, err := parsePermuted(fs, args)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if got != "traces.json" || *renderDir != "out" || !*update {
			t.Errorf("args %v: positional=%q render-dir=%q update=%v", args, got, *renderDir, *update)
		}
	}
	// More than one positional is an error.
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	if _, err := parsePermuted(fs, []string{"a.json", "b.json"}); err == nil {
		t.Error("two positionals should error")
	}
}

func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixtures", "loansvc")
}

// silenceStdout redirects os.Stdout for the duration of the test so a command's
// JSON output does not pollute the test log.
func silenceStdout(t *testing.T) {
	t.Helper()
	orig := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = orig
		_ = devnull.Close()
	})
}

func TestRunVersion(t *testing.T) {
	silenceStdout(t)
	if err := run([]string{"version"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	if err := run([]string{"frobnicate"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}

// TestRunBoundaryCheckCurrent verifies the gate passes against the fixture's
// committed contract.
func TestRunBoundaryCheckCurrent(t *testing.T) {
	silenceStdout(t)
	if err := run([]string{"boundary", "--check", fixtureDir()}); err != nil {
		t.Fatalf("boundary --check on a current fixture should pass: %v", err)
	}
}

func TestRunGraph(t *testing.T) {
	silenceStdout(t)
	if err := run([]string{"graph", "--entry", "POST /loan-application", fixtureDir()}); err != nil {
		t.Fatalf("graph: %v", err)
	}
}

// The graph header carries the PRODUCER version (the `tool` field, R11): flowmap
// stamps its own buildinfo.Version so groundwork can flag a base/branch built by
// two flowmap builds. It is derived at the CLI boundary, not in graphio.Build, so
// the field is present in CLI output but absent from a Build-only graph (which must
// stay a pure function of its inputs — the determinism test and goldens depend on it).
func TestRunGraphStampsToolVersion(t *testing.T) {
	out := captureStdout(t, func() {
		if err := run([]string{"graph", fixtureDir()}); err != nil {
			t.Fatalf("graph: %v", err)
		}
	})
	want := `"tool": ` + strconv.Quote(buildinfo.Version(version))
	if !strings.Contains(out, want) {
		t.Errorf("graph output must carry the producer version %s; got header:\n%s", want, firstLines(out, 6))
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestRunGraphAlgo: --algo selects the call-graph algorithm. rta/vta/cha all
// build; an unknown value is rejected before any analysis runs.
func TestRunGraphAlgo(t *testing.T) {
	silenceStdout(t)
	for _, a := range []string{"rta", "vta", "cha"} {
		if err := run([]string{"graph", "--algo", a, fixtureDir()}); err != nil {
			t.Fatalf("graph --algo %s: %v", a, err)
		}
	}
	if err := run([]string{"graph", "--algo", "bogus", fixtureDir()}); err == nil {
		t.Fatal("expected an error for an unknown --algo value")
	}
}

// TestRunBoundaryCheckStale verifies the gate fails when no contract is committed.
func TestRunBoundaryCheckStale(t *testing.T) {
	silenceStdout(t)
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write(t, dir, "go.mod", "module svc\n\ngo 1.24\n")
	write(t, dir, ".flowmap.yaml", "version: 1\nservice: svc\nclassify:\n  busPublish: [\"svc/bus#Publish\"]\n")
	write(t, dir, "bus/bus.go", "package bus\nfunc Publish(event string) {}\n")
	write(t, dir, "main.go", "package main\nimport \"svc/bus\"\nfunc main() { bus.Publish(\"x\") }\n")

	if err := run([]string{"boundary", "--check", dir}); err == nil {
		t.Fatal("boundary --check with no committed contract should fail")
	}
	// Writing it, then checking, should pass.
	if err := run([]string{"boundary", dir}); err != nil {
		t.Fatalf("boundary write: %v", err)
	}
	if err := run([]string{"boundary", "--check", dir}); err != nil {
		t.Fatalf("boundary --check after write should pass: %v", err)
	}
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDiffIdentical(t *testing.T) {
	silenceStdout(t)
	g := filepath.Join(fixtureDir(), "flows", "testdata", "flows", "post_loan_application.golden.json")
	if err := run([]string{"diff", g, g}); err != nil {
		t.Fatalf("identical traces should diff cleanly, got: %v", err)
	}
}

func TestRunDiffDiffers(t *testing.T) {
	silenceStdout(t)
	dir := filepath.Join(fixtureDir(), "flows", "testdata", "flows")
	a := filepath.Join(dir, "post_loan_application.golden.json")
	b := filepath.Join(dir, "consume_payment_settled.golden.json")
	if err := run([]string{"diff", a, b}); err == nil {
		t.Fatal("expected a non-nil error (non-zero exit) when traces differ")
	}
}

func TestRunDiffBadArgs(t *testing.T) {
	if err := run([]string{"diff", "only-one.json"}); err == nil {
		t.Fatal("expected an error when not given exactly two files")
	}
}

func TestRunCoverage(t *testing.T) {
	silenceStdout(t)
	flowsDir := filepath.Join(fixtureDir(), "flows", "testdata", "flows")
	// coverage is informational (exit 0) even when it finds unexercised effects.
	if err := run([]string{"coverage", "--flows", flowsDir, fixtureDir()}); err != nil {
		t.Fatalf("coverage on the fixture should succeed: %v", err)
	}
}

// coverage against a directory with no *.golden.json flow snapshots must not read
// as a clean pass: it discloses that it checked nothing, and — when the directory
// holds post-hoc *.effects.json goldens instead — names the format mismatch (F7).
func TestRunCoverageNoFlowsDiscloses(t *testing.T) {
	flowsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(flowsDir, "x.effects.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := run([]string{"coverage", "--flows", flowsDir, fixtureDir()}); err != nil {
			t.Fatalf("coverage stays informational (exit 0): %v", err)
		}
	})
	if strings.Contains(out, "every boundary effect is exercised") {
		t.Fatalf("zero flows must not read as full coverage:\n%s", out)
	}
	if !strings.Contains(out, "checked nothing") || !strings.Contains(out, "effects.json") {
		t.Fatalf("want a 'checked nothing' disclosure naming the effects.json mismatch:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// otlpFixture is the committed OTLP/JSON trace export used by the post-hoc tests.
func otlpFixture() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "otlp", "loan-application.otlp.json")
}

// TestBehaviorIngestCorpusDir exercises the Track-2 impeach-corpus exporter:
// --corpus-dir persists each canonicalized fragment as a STAMPLESS
// <flow>.<service>.golden.json — the exact committed-corpus form
// loadCommittedCorpus/ir.Load reads (it round-trips as a canonical trace, with the
// run-varying code-identity Stamp absent). A from-collector export carries no
// in-process flowmap.fqn tag, so the localization caveat must disclose the L0
// fallback rather than silently imply L1 precision.
func TestBehaviorIngestCorpusDir(t *testing.T) {
	dir := t.TempDir()
	fx := otlpFixture()

	out := captureStdout(t, func() {
		if err := run([]string{"behavior", "ingest", "--corpus-dir", dir, fx}); err != nil {
			t.Fatalf("ingest --corpus-dir: %v", err)
		}
	})

	gp := filepath.Join(dir, "loan_application.loansvc.golden.json")
	b, err := os.ReadFile(gp)
	if err != nil {
		t.Fatalf("expected impeach corpus golden at %s: %v", gp, err)
	}
	tr, err := ir.Load(b)
	if err != nil {
		t.Fatalf("corpus golden must load as a canonical trace (the impeach corpus form): %v", err)
	}
	if tr.Root == nil {
		t.Error("corpus golden has no root span")
	}
	if tr.Stamp != "" {
		t.Errorf("a committed corpus golden must be stampless; got stamp %q", tr.Stamp)
	}
	if _, err := os.Stat(filepath.Join(dir, "loan_application.loansvc.flow.md")); err != nil {
		t.Errorf("expected rendered view alongside the corpus golden: %v", err)
	}
	if !strings.Contains(out, "L0") || !strings.Contains(out, "flowmap.fqn") {
		t.Errorf("the L0 localization caveat must be disclosed for a from-collector corpus; got:\n%s", out)
	}
}

// TestBehaviorIngestGate exercises the stage-2 opt-in gate end to end: --update
// writes the effect golden; a re-gate of the same trace passes; dropping an
// effect from the golden makes that effect read as a new addition and fails
// (no-new-effects, D-PH3); and a golden with no capture this run is skipped, not
// silently passed (D-PH2).
func TestBehaviorIngestGate(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	fx := otlpFixture()

	// --update writes <slug>.<service>.effects.json (+ .flow.md).
	if err := run([]string{"behavior", "ingest", "--flows-dir", dir, "--update", fx}); err != nil {
		t.Fatalf("update: %v", err)
	}
	gp := filepath.Join(dir, "loan_application.loansvc.effects.json")
	if _, err := os.Stat(gp); err != nil {
		t.Fatalf("expected golden at %s: %v", gp, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "loan_application.loansvc.flow.md")); err != nil {
		t.Fatalf("expected rendered view: %v", err)
	}

	// Re-gating the same trace passes: no new effects.
	if err := run([]string{"behavior", "ingest", "--flows-dir", dir, fx}); err != nil {
		t.Fatalf("clean gate should pass: %v", err)
	}

	// Drop an effect from the golden; the trace now exercises one the golden lacks
	// → a new effect → the gate fails.
	g, err := ingest.LoadEffectGolden(gp)
	if err != nil {
		t.Fatal(err)
	}
	kept := g.Effects[:0:0]
	for _, e := range g.Effects {
		if e != "PUBLISH loan.approved" {
			kept = append(kept, e)
		}
	}
	g.Effects = kept
	b, _ := g.Marshal()
	if err := os.WriteFile(gp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"behavior", "ingest", "--flows-dir", dir, fx}); err == nil {
		t.Fatal("expected the gate to fail on a new boundary effect")
	}

	// D-PH2: a golden for a flow with no capture this run is skipped, not failed.
	// Restore the loan golden so it passes, add an orphan golden with no trace.
	full := ingest.NewEffectGolden("loan-application", "loansvc",
		[]string{"HTTP GET credit-bureau /score/{id}", "HTTP POST /loan-application", "PUBLISH loan.approved"})
	fb, _ := full.Marshal()
	if err := os.WriteFile(gp, fb, 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := ingest.NewEffectGolden("nightly-sweep", "other-svc", []string{"PUBLISH sweep.done"})
	ob, _ := orphan.Marshal()
	if err := os.WriteFile(filepath.Join(dir, "nightly_sweep.other_svc.effects.json"), ob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"behavior", "ingest", "--flows-dir", dir, fx}); err != nil {
		t.Fatalf("a golden with no capture should be skipped, not fail the gate: %v", err)
	}
}

// otlpDoc writes a one-resource OTLP/JSON file with the given spans and returns
// its path. Each span: id, parent, kind, flow slug, and extra attrs.
func writeOTLP(t *testing.T, dir, name, service string, spans string) string {
	t.Helper()
	doc := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"` +
		service + `"}}]},"scopeSpans":[{"spans":[` + spans + `]}]}]}`
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func producerSpan(id, parent, slug, topic string) string {
	return `{"spanId":"` + id + `","parentSpanId":"` + parent + `","name":"p","kind":4,"attributes":[` +
		`{"key":"flowmap.flow","value":{"stringValue":"` + slug + `"}},` +
		`{"key":"messaging.destination.name","value":{"stringValue":"` + topic + `"}}],"status":{"code":1}}`
}

// TestBehaviorIngestSkipsSynthesized: a fragment with no clean inbound entry span
// (synthesized root — completeness unverifiable) is skipped by the gate, not
// passed, even when it carries an effect absent from the golden (finding #1).
func TestBehaviorIngestSkipsSynthesized(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()

	// Publisher-only flow (parent outside the capture → synthesized root).
	t1 := writeOTLP(t, dir, "t1.json", "emitter", producerSpan("01", "ffffffffffffffff", "sweep", "a.done"))
	if err := run([]string{"behavior", "ingest", "--flows-dir", dir, "--update", t1}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// A later run where the same synthesized flow adds a brand-new effect.
	t2 := writeOTLP(t, dir, "t2.json", "emitter",
		producerSpan("01", "ffffffffffffffff", "sweep", "a.done")+","+producerSpan("02", "ffffffffffffffff", "sweep", "b.new"))
	// Without the completeness guard this would fail with [CONTRACT] ADDED b.new;
	// because the capture has no inbound entry, it is skipped instead.
	if err := run([]string{"behavior", "ingest", "--flows-dir", dir, t2}); err != nil {
		t.Fatalf("synthesized fragment must be skipped, not gated: %v", err)
	}
}

// TestUpdateGoldenCollision: two distinct flows whose slugs collide to one
// filename are refused on --update rather than silently overwriting (finding #2).
func TestUpdateGoldenCollision(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	tf := writeOTLP(t, dir, "t.json", "svc",
		producerSpan("01", "ffffffffffffffff", "sweep-a", "x")+","+producerSpan("02", "ffffffffffffffff", "sweep.a", "y"))
	err := run([]string{"behavior", "ingest", "--flows-dir", dir, "--update", tf})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected a slug-collision error for sweep-a vs sweep.a, got %v", err)
	}
}

// TestBehaviorIngestRenderDir: --render-dir emits the cross-service view in any
// mode (here stage 1, no gate), --root writes a service-centric diagram, and
// --root without --render-dir is rejected.
func TestBehaviorIngestRenderDir(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	trace := `{"resourceSpans":[
      {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"loansvc"}}]},"scopeSpans":[{"spans":[
        {"spanId":"01","parentSpanId":"","name":"e","kind":2,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"http.route","value":{"stringValue":"/x"}}],"status":{"code":1}},
        {"spanId":"02","parentSpanId":"01","name":"c","kind":3,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"peer.service","value":{"stringValue":"bureau"}},{"key":"http.request.method","value":{"stringValue":"GET"}},{"key":"http.route","value":{"stringValue":"/s"}}],"status":{"code":1}}
      ]}]},
      {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"bureau"}}]},"scopeSpans":[{"spans":[
        {"spanId":"03","parentSpanId":"02","name":"s","kind":2,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"http.request.method","value":{"stringValue":"GET"}},{"key":"http.route","value":{"stringValue":"/s"}}],"status":{"code":1}}
      ]}]}
    ]}`
	tf := filepath.Join(dir, "t.json")
	if err := os.WriteFile(tf, []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := run([]string{"behavior", "ingest", "--render-dir", out, tf}); err != nil {
		t.Fatalf("stage-1 render: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "loan.system.flow.md")); err != nil {
		t.Fatalf("expected whole-flow diagram: %v", err)
	}
	if err := run([]string{"behavior", "ingest", "--render-dir", out, "--root", "bureau", tf}); err != nil {
		t.Fatalf("rooted render: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "loan.bureau.system.flow.md")); err != nil {
		t.Fatalf("expected service-rooted diagram: %v", err)
	}
	if err := run([]string{"behavior", "ingest", "--root", "bureau", tf}); err == nil {
		t.Fatal("expected an error: --root without --render-dir")
	}
}

// TestBehaviorIngestMerged: --merged writes a single system.context.md, and the
// flag dependencies are validated.
func TestBehaviorIngestMerged(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	trace := `{"resourceSpans":[
      {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"loansvc"}}]},"scopeSpans":[{"spans":[
        {"spanId":"01","parentSpanId":"","name":"e","kind":2,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"http.route","value":{"stringValue":"/x"}}],"status":{"code":1}},
        {"spanId":"05","parentSpanId":"01","name":"p","kind":4,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"messaging.destination.name","value":{"stringValue":"loan.approved"}}],"status":{"code":1}}
      ]}]}
    ]}`
	tf := filepath.Join(dir, "t.json")
	if err := os.WriteFile(tf, []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := run([]string{"behavior", "ingest", "--render-dir", out, "--merged", tf}); err != nil {
		t.Fatalf("merged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "system.context.md")); err != nil {
		t.Fatalf("expected system.context.md: %v", err)
	}
	// --merged requires --render-dir; --choreography requires --merged.
	if err := run([]string{"behavior", "ingest", "--merged", tf}); err == nil {
		t.Fatal("expected error: --merged without --render-dir")
	}
	if err := run([]string{"behavior", "ingest", "--render-dir", out, "--choreography", tf}); err == nil {
		t.Fatal("expected error: --choreography without --merged")
	}
}

// TestBehaviorIngestContractSkipsBadDir: a --contracts overlay dir that fails to
// load is a non-gated view concern — it must warn-and-skip, not fail the ingest.
func TestBehaviorIngestContractSkipsBadDir(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	trace := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"loansvc"}}]},"scopeSpans":[{"spans":[
        {"spanId":"01","parentSpanId":"","name":"e","kind":2,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"http.route","value":{"stringValue":"/x"}}],"status":{"code":1}}
      ]}]}]}`
	tf := filepath.Join(dir, "t.json")
	if err := os.WriteFile(tf, []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := run([]string{"behavior", "ingest", "--render-dir", out, "--merged", "--contracts", filepath.Join(dir, "nonexistent"), tf}); err != nil {
		t.Fatalf("a bad --contracts dir must warn+skip, not error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "system.context.md")); err != nil {
		t.Fatalf("system.context.md should still be written: %v", err)
	}
}

// TestBehaviorIngestSynthesizedLifeline: a multi-entry whole flow (two top-level
// entries under one slug → synthesized root) must not render an unnamed
// participant; the lifeline falls back to the slug.
func TestBehaviorIngestSynthesizedLifeline(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	trace := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"loansvc"}}]},"scopeSpans":[{"spans":[
        {"spanId":"01","parentSpanId":"","name":"a","kind":2,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"http.route","value":{"stringValue":"/a"}}],"status":{"code":1}},
        {"spanId":"02","parentSpanId":"","name":"b","kind":2,"attributes":[{"key":"flowmap.flow","value":{"stringValue":"loan"}},{"key":"http.request.method","value":{"stringValue":"POST"}},{"key":"http.route","value":{"stringValue":"/b"}}],"status":{"code":1}}
      ]}]}]}`
	tf := filepath.Join(dir, "t.json")
	if err := os.WriteFile(tf, []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := run([]string{"behavior", "ingest", "--render-dir", out, tf}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "loan.system.flow.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `as ""`) {
		t.Errorf("synthesized root rendered an unnamed participant:\n%s", b)
	}
	if !strings.Contains(string(b), "loan") {
		t.Errorf("expected the slug as the fallback lifeline:\n%s", b)
	}
}
