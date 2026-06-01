package flow_test

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jyang234/golang-code-graph/flow"
	"github.com/jyang234/golang-code-graph/harness"
)

// loanSUT is a miniature, OTel-instrumented loan service driven through the
// public harness exactly as a target repo would drive its own router. The
// applicant read and the credit-score call race (both perform real I/O), then a
// sequential charge → publish → ledger, and a fire-and-forget audit write after
// the response.
func loanSUT(extraPublishes ...string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /loan-application", func(w http.ResponseWriter, r *http.Request) {
		tr := otel.Tracer("loansvc")
		ctx := r.Context()

		evalCtx, eval := tr.Start(ctx, "evaluateApplication")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, sp := tr.Start(evalCtx, "select", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
			sp.SetAttributes(
				attribute.String("db.system", "postgresql"),
				attribute.String("db.statement", "SELECT name, income FROM applicants WHERE id = $1"),
			)
			time.Sleep(8 * time.Millisecond)
			sp.End()
		}()
		go func() {
			defer wg.Done()
			scCtx, sc := tr.Start(evalCtx, "scorer.Score")
			_, b := tr.Start(scCtx, "GET /score", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
			b.SetAttributes(
				attribute.String("http.request.method", "GET"),
				attribute.String("peer.service", "credit-bureau"),
				attribute.String("http.target", "/score/8412"),
			)
			time.Sleep(8 * time.Millisecond)
			b.End()
			sc.End()
		}()
		wg.Wait()
		eval.End()

		_, ch := tr.Start(ctx, "charge", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
		ch.SetAttributes(
			attribute.String("http.request.method", "POST"),
			attribute.String("peer.service", "payment-gw"),
			attribute.String("http.target", "/charge/8412"),
		)
		ch.End()

		_, pub := tr.Start(ctx, "publish", oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
		pub.SetAttributes(attribute.String("messaging.destination.name", "loan.approved"))
		pub.End()

		// extraPublishes inject behavioral drift (a new published event) for the
		// gate-fails-on-drift test; the canonical SUT passes none.
		for _, event := range extraPublishes {
			_, ep := tr.Start(ctx, "publish", oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
			ep.SetAttributes(attribute.String("messaging.destination.name", event))
			ep.End()
		}

		_, led := tr.Start(ctx, "ledger", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
		led.SetAttributes(
			attribute.String("db.system", "postgres"),
			attribute.String("db.statement", "INSERT INTO ledger (loan_id, amount) VALUES ($1, $2)"),
		)
		led.End()

		auditCtx := context.WithoutCancel(ctx)
		go func() {
			time.Sleep(3 * time.Millisecond)
			_, au := tr.Start(auditCtx, "audit", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
			au.SetAttributes(
				attribute.String("db.system", "postgres"),
				attribute.String("db.statement", "INSERT INTO audit_log (loan_id) VALUES ($1)"),
			)
			au.End()
		}()

		w.WriteHeader(http.StatusOK)
	})
	return mux
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
