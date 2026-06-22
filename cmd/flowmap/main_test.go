package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/buildinfo"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/impeach"
	"github.com/jyang234/golang-code-graph/internal/ingest"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/ir"
)

// TestWarnSkippedAnnotations pins the §22 CLI warning: a build that warn-and-skipped
// an algorithm-fragile annotation (recorded on g.SkippedAnnotations) emits a stderr
// warning naming the site, the absent kind, the --algo, and the fix — and NOTHING
// when there is nothing to skip (a clean build is silent). The skip itself is decided
// in graphio.Build; this only covers the boundary surface.
func TestWarnSkippedAnnotations(t *testing.T) {
	var buf bytes.Buffer
	g := &graphio.Graph{Algo: "rta", SkippedAnnotations: []graphio.SkippedAnnotation{
		{Site: "svc.Send", Kind: "UnresolvedCall", Present: []string{"ExternalBoundaryCall"}},
	}}
	warnSkippedAnnotations(&buf, g)
	out := buf.String()
	for _, want := range []string{"warning", "svc.Send", "UnresolvedCall", "rta", "ExternalBoundaryCall", "algorithm-dependent", "vta"} {
		if !strings.Contains(out, want) {
			t.Errorf("skip warning missing %q:\n%s", want, out)
		}
	}

	// A build with nothing skipped is silent — no warning noise on the common path.
	buf.Reset()
	warnSkippedAnnotations(&buf, &graphio.Graph{Algo: "rta"})
	if buf.Len() != 0 {
		t.Errorf("a clean build must emit no warning, got: %s", buf.String())
	}
}

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

// TestRunSchemaDrift exercises the schema-drift subcommand end-to-end: it reads an
// emitted graph JSON + a migrations dir and flags a code write to a table no
// migration defines, while a defined table reads clean. Also pins the required-flag
// guards.
func TestRunSchemaDrift(t *testing.T) {
	dir := t.TempDir()

	g := graphio.Graph{Edges: []graphio.Edge{
		{From: "svc.A", To: "boundary:db INSERT defined_table"},
		{From: "svc.B", To: "boundary:db INSERT ghost_table"},
	}}
	gb, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	gpath := filepath.Join(dir, "graph.json")
	if err := os.WriteFile(gpath, gb, 0o644); err != nil {
		t.Fatal(err)
	}

	mdir := filepath.Join(dir, "migrations")
	if err := os.Mkdir(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mdir, "V1__init.sql"), []byte("CREATE TABLE defined_table (id text);"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := run([]string{"schema-drift", "--graph", gpath, "--migrations", mdir, "--json"}); err != nil {
			t.Fatalf("schema-drift: %v", err)
		}
	})
	if !strings.Contains(out, "ghost_table") {
		t.Errorf("expected ghost_table drift in JSON, got: %s", out)
	}
	if strings.Contains(out, "defined_table") {
		t.Errorf("defined_table is defined and must not drift, got: %s", out)
	}

	// With no migrations source (no flag, no config) the check errors rather than
	// guessing.
	if err := run([]string{"schema-drift", "--graph", gpath}); err == nil {
		t.Error("expected an error when no migrations source is given")
	}
}

// TestRunSchemaDriftBuildFresh exercises the one-step CI form: no --graph, so the
// graph is built fresh from the service dir, whose .flowmap.yaml supplies the
// migrations dir and library-owned set. --gate then turns drift into a non-zero exit.
func TestRunSchemaDriftBuildFresh(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	svc := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixtures", "schemadriftsvc")

	out := captureStdout(t, func() {
		if err := run([]string{"schema-drift", svc}); err != nil {
			t.Fatalf("schema-drift build-fresh: %v", err)
		}
	})
	if !strings.Contains(out, "audit_log") || !strings.Contains(out, "queue_messages") {
		t.Errorf("expected drift on audit_log and queue_messages, got:\n%s", out)
	}
	// provisioning_outbox is declared library-owned in the fixture config, so it must
	// not drift here.
	if strings.Contains(out, "- provisioning_outbox ") {
		t.Errorf("provisioning_outbox is library-owned and must not drift, got:\n%s", out)
	}

	// --gate exits non-zero when drift is present.
	silenceStdout(t)
	if err := run([]string{"schema-drift", "--gate", svc}); err == nil {
		t.Error("--gate should error when drift is present")
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

// TestRunGraphRollup: --rollup package emits the component (C3) view as JSON, and as a
// Mermaid flowchart under --mermaid; an unknown rollup kind and the --root combination
// are rejected before any output.
func TestRunGraphRollup(t *testing.T) {
	jsonOut := captureStdout(t, func() {
		if err := run([]string{"graph", "--rollup", "package", fixtureDir()}); err != nil {
			t.Fatalf("graph --rollup package: %v", err)
		}
	})
	if !strings.Contains(jsonOut, `"components"`) || !strings.Contains(jsonOut, `"kind": "call"`) {
		t.Errorf("rollup JSON must carry components and call edges; got:\n%s", firstLines(jsonOut, 8))
	}

	mermaidOut := captureStdout(t, func() {
		if err := run([]string{"graph", "--rollup", "package", "--mermaid", fixtureDir()}); err != nil {
			t.Fatalf("graph --rollup package --mermaid: %v", err)
		}
	})
	if !strings.Contains(mermaidOut, "flowchart LR") || !strings.Contains(mermaidOut, "component (C3) rollup") {
		t.Errorf("rollup --mermaid must render a component flowchart; got:\n%s", firstLines(mermaidOut, 5))
	}

	silenceStdout(t)
	if err := run([]string{"graph", "--rollup", "bogus", fixtureDir()}); err == nil {
		t.Error(`--rollup bogus must be rejected (only "package" is supported)`)
	}
	if err := run([]string{"graph", "--rollup", "package", "--root", "POST /loan-application", fixtureDir()}); err == nil {
		t.Error("--rollup and --root are mutually exclusive and must be rejected")
	}
	if err := run([]string{"graph", "--rollup", "package", "--entry", "POST /loan-application", fixtureDir()}); err == nil {
		t.Error("--rollup and --entry are mutually exclusive (whole-service view) and must be rejected")
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

// crossServiceFixture is a 2-resource OTLP/JSON trace: impeachsvc (entry + outbound
// client) propagating into peersvc (server + two DB DELETEs). The peer's clock is
// skewed ~0.5s behind impeachsvc's, and its two sibling DB writes are timestamped
// peer_ledger-before-peer_audit — the REVERSE of canonical op-key order — so a sound
// out-of-process canonicalization must order them by op key, never by the misleading
// cross-clock-domain intervals. It is GENERATED by the OTel Collector's own
// ptrace.JSONMarshaler via testdata/otlpgen (like loansvc.collector.otlp.json), so its
// wire format is real collector output, not a hand-authored guess — regenerate with
// `cd testdata/otlpgen && GOWORK=off go run . crossservice > ../otlp/cross_service_peer.otlp.json`.
const crossServiceFixture = "../../testdata/otlp/cross_service_peer.otlp.json"

// TestCrossServiceImpeachFromOTLP closes the §17 cross-service residual end to end on
// the REAL out-of-process path — not the in-process tagged-span model. A multi-resource,
// collector-shaped OTLP trace is ingested via `behavior ingest --corpus-dir` (the adopter
// command) through otlpjson → ingest → canon's postHoc profile, then audited against the
// impeachsvc graph: the peer's DB writes are owned by peersvc, so they downgrade to
// CROSS-SERVICE. This exercises what the tagged-span fixture could not — multi-resource
// service split, the postHoc cross-clock-domain ordering, and the service-scope rung over
// a genuinely out-of-process corpus. The only remaining residual is two live processes +
// a real collector merge: capture-stack plumbing outside impeach's trust boundary, whose
// OTLP bytes are structurally identical to this fixture.
func TestCrossServiceImpeachFromOTLP(t *testing.T) {
	silenceStdout(t)
	dir := t.TempDir()
	if err := run([]string{"behavior", "ingest", "--corpus-dir", dir, crossServiceFixture}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Cross-clock-domain ordering: canon's postHoc profile ordered the peer's two DB
	// siblings by op key (peer_audit before peer_ledger), NOT by their misleading
	// timestamps (peer_ledger starts earlier). This is the guard that caller-clock
	// interval overlap is never used across clock domains.
	peerGolden := filepath.Join(dir, "admin_federate.peersvc.golden.json")
	b, err := os.ReadFile(peerGolden)
	if err != nil {
		t.Fatalf("read peer golden: %v", err)
	}
	if i, j := bytes.Index(b, []byte("peer_audit")), bytes.Index(b, []byte("peer_ledger")); i < 0 || j < 0 || i > j {
		t.Errorf("postHoc did not order the peer siblings by op key (audit before ledger) — cross-clock-domain interval ordering leaked:\n%s", b)
	}

	// Determinism over the skewed clocks: re-ingesting yields a byte-identical corpus.
	dir2 := t.TempDir()
	if err := run([]string{"behavior", "ingest", "--corpus-dir", dir2, crossServiceFixture}); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if b2, _ := os.ReadFile(filepath.Join(dir2, "admin_federate.peersvc.golden.json")); !bytes.Equal(b, b2) {
		t.Error("corpus golden not deterministic across ingests of the skewed-clock trace")
	}

	// The audit: load the whole ingested corpus and audit against the impeachsvc graph
	// under provenance, so the only open rung is service-scope.
	g, err := graph.LoadFile("../../internal/impeach/testdata/impeachsvc.graph.json")
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	g.Stamp = "deadbeefcafe"
	r := impeach.Audit("impeachsvc", graph.NewIndex(g), loadCorpusGoldens(t, dir), impeach.Provenance{TraceIdentity: "deadbeefcafe"})

	got := map[string]string{}
	for _, w := range r.Candidates {
		got[w.Effect] = w.Verdict
		if w.Observed.Service != "peersvc" {
			t.Errorf("candidate %q service = %q, want peersvc (the foreign owner)", w.Effect, w.Observed.Service)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want exactly the 2 peer DB effects as candidates, got %d: %+v", len(got), r.Candidates)
	}
	for _, eff := range []string{"db DELETE peer_ledger", "db DELETE peer_audit"} {
		if got[eff] != impeach.DowngradeCrossService {
			t.Errorf("verdict for %q = %q, want %q (effect on the peer's service span)", eff, got[eff], impeach.DowngradeCrossService)
		}
	}
}

// loadCorpusGoldens loads every *.golden.json under dir as a canonical trace.
func loadCorpusGoldens(t *testing.T, dir string) []*ir.CanonicalTrace {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.golden.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no goldens under %s: %v", dir, err)
	}
	var out []*ir.CanonicalTrace
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		tr, err := ir.Load(b)
		if err != nil {
			t.Fatalf("load %s: %v", p, err)
		}
		out = append(out, tr)
	}
	return out
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

// TestLoadGraphJSONStrict pins the --diff base forward-compatibility guard: a base
// from a NEWER flowmap (an unknown field) is rejected rather than silently decoded
// with that field dropped, which would produce a confidently-wrong delta. A clean
// same-version graph still loads.
func TestLoadGraphJSONStrict(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"algo":"rta","nodes":[{"fqn":"a.F","tier":1}],"edges":[],"blind_spots":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGraphJSON(good); err != nil {
		t.Errorf("a clean same-version graph must load: %v", err)
	}

	newer := filepath.Join(dir, "newer.json")
	if err := os.WriteFile(newer, []byte(`{"algo":"rta","nodes":[],"edges":[],"blind_spots":[],"future_field":42}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGraphJSON(newer); err == nil {
		t.Error("a base with an unknown field (newer flowmap) must be rejected, not silently decoded")
	}
}
