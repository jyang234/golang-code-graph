package flow_test

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jyang234/golang-code-graph/flow"
	"github.com/jyang234/golang-code-graph/harness"
	"github.com/jyang234/golang-code-graph/internal/loansut"
)

// loanSUT is the shared loan SUT (internal/loansut) driven through the public
// harness exactly as a target repo would drive its own router. extraPublishes
// inject behavioral drift (a new published event) for the gate-fails-on-drift
// test; the canonical SUT passes none.
func loanSUT(extraPublishes ...string) http.Handler {
	return loansut.Handler(loansut.Options{ExtraPublishes: extraPublishes})
}

// TestLoanApplicationFlow is the headline acceptance: a target-style flow test
// that compiles against only the public harness/flow packages and gates inside
// plain `go test`. The committed golden under testdata/flows is the assertion;
// run with -update to rebase it.
func TestLoanApplicationFlow(t *testing.T) {
	app := harness.NewInProcess(t, loanSUT(), harness.WithService("loansvc"))
	flow.New("POST /loan-application").
		Trigger("POST", "/loan-application").
		ExpectExactlyOnce("HTTP POST payment-gw /charge/{id}").
		ExpectExactlyOnce("PUBLISH loan.approved").
		Expect("DB postgres INSERT ledger").
		Expect("DB postgres INSERT audit_log").
		Quiescence(10*time.Millisecond, 2*time.Second).
		Run(t, app)
}

// recordingTB captures Errorf/Fatalf so a test can assert the DSL fails for the
// right reason. Fatalf unwinds via panic, mirroring testing.T's Goexit.
type recordingTB struct {
	errs  []string
	fatal string
}

func (r *recordingTB) Helper()                   {}
func (r *recordingTB) Logf(string, ...any)       {}
func (r *recordingTB) Errorf(f string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(f, a...)) }
func (r *recordingTB) Fatalf(f string, a ...any) { r.fatal = fmt.Sprintf(f, a...); panic(r) }

// TestCardinalityViolationFailsEvenWhenGoldenMatches drives a flow that publishes
// twice but declares the publish ExpectExactlyOnce. With -update forced on, the
// golden is (re)written and matches, yet the cardinality assertion still fails —
// proving the check is independent of snapshot equality.
func TestCardinalityViolationFailsEvenWhenGoldenMatches(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) {
		tr := otel.Tracer("svc")
		for i := 0; i < 2; i++ {
			_, p := tr.Start(r.Context(), "publish", oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
			p.SetAttributes(attribute.String("messaging.destination.name", "loan.approved"))
			p.End()
		}
		w.WriteHeader(http.StatusOK)
	})
	app := harness.NewInProcess(t, mux, harness.WithService("svc"))

	// Force the golden to be (re)written so it matches, isolating the cardinality
	// failure from any snapshot mismatch.
	restore := setUpdate(t, true)
	defer restore()

	rec := &recordingTB{}
	func() {
		defer func() { _ = recover() }() // Fatalf unwinds via panic; ignore.
		flow.New("double publish").
			Trigger("POST", "/x").
			ExpectExactlyOnce("PUBLISH loan.approved").
			GoldensDir(t.TempDir()).
			Quiescence(10*time.Millisecond, time.Second).
			Run(rec, app)
	}()

	// The two identical publishes collapse to a 1..* loop, so the cardinality
	// check reports a collapsed loop — still a violation of ExpectExactlyOnce.
	if !anyContains(rec.errs, "ExpectExactlyOnce") || !anyContains(rec.errs, "1..*") {
		t.Fatalf("expected a cardinality violation, got errs=%v fatal=%q", rec.errs, rec.fatal)
	}
	for _, e := range rec.errs {
		if strings.Contains(e, "does not match") {
			t.Errorf("golden should have matched (update was forced), but saw a mismatch: %s", e)
		}
	}
}

// TestBehavioralGateFailsOnDrift is the inject-drift proof for the
// snapshot-assertion gate: a flow that publishes a new event no longer matches
// the committed golden, and Run fails with the prioritized structural change set
// (the new publish ranked as a [CONTRACT] change), not a raw text diff. The
// canonical TestLoanApplicationFlow proves the complementary "passes when
// current" half.
func TestBehavioralGateFailsOnDrift(t *testing.T) {
	app := harness.NewInProcess(t, loanSUT("loan.flagged"), harness.WithService("loansvc"))
	rec := &recordingTB{}
	func() {
		defer func() { _ = recover() }()
		flow.New("POST /loan-application").
			Trigger("POST", "/loan-application").
			Expect("HTTP GET credit-bureau /score/{id}").
			Expect("PUBLISH loan.approved").
			Expect("PUBLISH loan.flagged").
			Expect("DB postgres INSERT ledger").
			Expect("DB postgres INSERT audit_log").
			Quiescence(15*time.Millisecond, 3*time.Second).
			Run(rec, app)
	}()
	if !anyContains(rec.errs, "does not match") {
		t.Fatalf("expected a golden mismatch, got errs=%v fatal=%q", rec.errs, rec.fatal)
	}
	if !anyContains(rec.errs, "[CONTRACT] ADDED PUBLISH loan.flagged") {
		t.Errorf("expected the drift reported as a prioritized contract change, got: %v", rec.errs)
	}
}

// TestTruncatedFlowFailsLoudly declares an exit that never occurs; Run must fail
// rather than gate a partial trace.
func TestTruncatedFlowFailsLoudly(t *testing.T) {
	app := harness.NewInProcess(t, loanSUT(), harness.WithService("loansvc"))
	rec := &recordingTB{}
	func() {
		defer func() { _ = recover() }()
		flow.New("never completes").
			Trigger("POST", "/loan-application").
			Expect("PUBLISH nonexistent.event").
			GoldensDir(t.TempDir()).
			Quiescence(5*time.Millisecond, 150*time.Millisecond).
			Run(rec, app)
	}()
	if rec.fatal == "" || !strings.Contains(rec.fatal, "capture failed") {
		t.Fatalf("expected a loud truncation failure, got fatal=%q errs=%v", rec.fatal, rec.errs)
	}
}

func setUpdate(t *testing.T, v bool) func() {
	t.Helper()
	f := flag.Lookup("update")
	if f == nil {
		t.Fatal("the -update flag is not registered")
	}
	prev := f.Value.String()
	if err := flag.Set("update", boolStr(v)); err != nil {
		t.Fatal(err)
	}
	return func() { _ = flag.Set("update", prev) }
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestSelfTestRunCount proves SelfTest(n) controls how many times Run re-drives
// the flow — the knob that lets a non-idempotent flow opt down to a single
// execution.
func TestSelfTestRunCount(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, p := otel.Tracer("svc").Start(r.Context(), "publish", oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
		p.SetAttributes(attribute.String("messaging.destination.name", "e"))
		p.End()
		w.WriteHeader(http.StatusOK)
	})
	app := harness.NewInProcess(t, mux, harness.WithService("svc"))
	restore := setUpdate(t, true) // write the golden so Compare passes; we only count drives
	defer restore()

	count := func(f *flow.Flow) int32 {
		atomic.StoreInt32(&calls, 0)
		f.Trigger("POST", "/x").Expect("PUBLISH e").
			GoldensDir(t.TempDir()).Quiescence(10*time.Millisecond, time.Second).Run(t, app)
		return atomic.LoadInt32(&calls)
	}

	if n := count(flow.New("once").SelfTest(1)); n != 1 {
		t.Errorf("SelfTest(1) drove the flow %d times, want 1", n)
	}
	if n := count(flow.New("default")); n != 3 {
		t.Errorf("default self-test drove the flow %d times, want 3", n)
	}
}

// TestFlowAppliesConfigTierPin is the regression for the cross-pipeline tier-map
// guarantee: a tier pin in the service's .flowmap.yaml must change the
// behavioral golden, not just the static contract. specialOp is a first-party
// internal op (tier 3, dropped at the warn threshold); pinning it to tier 1
// retains it in the snapshot. Without the config, it is dropped.
func TestFlowAppliesConfigTierPin(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) {
		tr := otel.Tracer("svc")
		_, s := tr.Start(r.Context(), "specialOp") // internal → tier 3 by default
		s.End()
		_, p := tr.Start(r.Context(), "publish", oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
		p.SetAttributes(attribute.String("messaging.destination.name", "e"))
		p.End()
		w.WriteHeader(http.StatusOK)
	})
	app := harness.NewInProcess(t, mux, harness.WithService("svc"))
	restore := setUpdate(t, true) // write goldens to temp dirs; we inspect them
	defer restore()

	goldenFor := func(configDir string) string {
		gdir := t.TempDir()
		flow.New("pin flow").
			Trigger("POST", "/x").
			Expect("PUBLISH e").
			ConfigDir(configDir).
			GoldensDir(gdir).
			Quiescence(10*time.Millisecond, time.Second).
			Run(t, app)
		b, err := os.ReadFile(filepath.Join(gdir, "pin_flow.golden.json"))
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		return string(b)
	}

	pinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pinDir, ".flowmap.yaml"),
		[]byte("pins:\n  - identity: \"specialOp\"\n    tier: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	withPin := goldenFor(pinDir)
	withoutPin := goldenFor(t.TempDir()) // empty dir → no config → defaults

	if !strings.Contains(withPin, "specialOp") {
		t.Errorf("a tier-1 pin should retain specialOp in the golden:\n%s", withPin)
	}
	if strings.Contains(withoutPin, "specialOp") {
		t.Errorf("without the pin, specialOp (tier 3) should be dropped:\n%s", withoutPin)
	}
}
