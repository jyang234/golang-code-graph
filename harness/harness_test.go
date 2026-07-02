package harness_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/harness"
	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/loansut"
	"github.com/jyang234/golang-code-graph/ir"
)

// instrumentedSUT is the shared loan SUT (internal/loansut) configured for the
// harness suite: slowLeg lengthens the credit-bureau call so the concurrency
// classification can be proven timing-stable.
func instrumentedSUT(slowLeg time.Duration) http.Handler {
	return loansut.Handler(loansut.Options{Tracer: "sut", SlowLeg: slowLeg})
}

func happyMarkers() []string {
	return []string{
		"HTTP GET credit-bureau /score/{id}",
		"PUBLISH loan.approved",
		"DB postgres INSERT ledger",
		"DB postgres INSERT audit_log",
	}
}

// TestCaptureDisclosesCorrelationLessSpan pins the M-18 disclosure end-to-end: a
// handler that opens a span from a fresh context.Background() (the lost-ctx bug)
// produces a correlation-less span that Scope excludes from the golden, and the
// harness must report it via CapturedFlow.CorrelationLess. This is the regression
// for the bug where the count was taken over the already-scoped set and was
// therefore always zero.
func TestCaptureDisclosesCorrelationLessSpan(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately NOT r.Context(): a fresh context carries no correlation
		// baggage, so this span cannot be attributed to the run.
		_, span := otel.Tracer("sut").Start(context.Background(), "orphan-work")
		span.End()
		w.WriteHeader(http.StatusOK)
	})
	app := harness.NewInProcess(t, handler, harness.WithService("svc"))
	cf, err := app.HTTP("POST", "/x", nil).Capture(harness.CaptureOptions{
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if cf.CorrelationLess < 1 {
		t.Errorf("CorrelationLess = %d, want >= 1 — the lost-context span must be disclosed, not silently zero", cf.CorrelationLess)
	}
	// The orphan span must not appear in the scoped golden set.
	for _, s := range cf.Spans {
		if s.Name == "orphan-work" {
			t.Error("correlation-less span leaked into the scoped Spans")
		}
	}
}

func TestHTTPCaptureComplete(t *testing.T) {
	app := harness.NewInProcess(t, instrumentedSUT(0), harness.WithService("loansvc"))
	cf, err := app.HTTP("POST", "/loan-application", []byte(`{"id":"8412"}`)).Capture(harness.CaptureOptions{
		Markers: happyMarkers(),
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !cf.Complete {
		t.Fatal("expected Complete=true")
	}
	if cf.Root == nil || cf.Root.Kind != ir.KindServer {
		t.Fatalf("root = %+v, want a server span", cf.Root)
	}
	if cf.Service != "loansvc" {
		t.Errorf("service = %q", cf.Service)
	}
	// The fire-and-forget audit span must have been drained in.
	if !hasSpanWithAttr(cf.Spans, "db.statement", "INSERT INTO audit_log (loan_id) VALUES ($1)") {
		t.Error("audit span from the goroutine was not captured")
	}
	// Sanity: a producer publish and a client DB read are present.
	if countKind(cf.Spans, ir.KindProducer) != 1 {
		t.Error("expected exactly one producer (publish) span")
	}
	if countKind(cf.Spans, ir.KindClient) < 3 {
		t.Error("expected the DB/client/charge spans")
	}
}

func TestHTTPCaptureCanonicalizes(t *testing.T) {
	app := harness.NewInProcess(t, instrumentedSUT(0), harness.WithService("loansvc"))
	cf, err := app.HTTP("POST", "/loan-application", nil).Capture(harness.CaptureOptions{
		Markers: happyMarkers(),
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := canon.Canonicalize(*cf, nil)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if tr.Root.Op != "HTTP POST /loan-application" {
		t.Errorf("root op = %q", tr.Root.Op)
	}
	// The tier-3 evaluator/scorer are contracted away; the applicant read and the
	// credit-bureau call surface as a concurrent pair directly under the root.
	g0 := tr.Root.Children[0]
	if !g0.Concurrent || len(g0.Members) != 2 {
		t.Fatalf("first group should be the promoted concurrent pair, got %+v", g0)
	}
	if g0.Members[0].Op != "DB postgresql SELECT applicants" || g0.Members[1].Op != "HTTP GET credit-bureau /score/{id}" {
		t.Errorf("concurrent members = %q, %q", g0.Members[0].Op, g0.Members[1].Op)
	}
}

// TestWithCodeStampCarriesToTrace proves the in-process capture path for the
// code-identity stamp: WithCodeStamp rides through the CapturedFlow onto the
// canonical trace's Stamp (the behavioral mirror of the graph's --stamp), so an
// in-process audit can establish the corpus identity. The default (no option)
// leaves the trace stampless.
func TestWithCodeStampCarriesToTrace(t *testing.T) {
	const commit = "deadbeefcafe"
	app := harness.NewInProcess(t, instrumentedSUT(0), harness.WithService("loansvc"), harness.WithCodeStamp(commit))
	cf, err := app.HTTP("POST", "/loan-application", nil).Capture(harness.CaptureOptions{
		Markers: happyMarkers(),
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cf.Stamp != commit {
		t.Errorf("CapturedFlow.Stamp = %q, want %q", cf.Stamp, commit)
	}
	tr, err := canon.Canonicalize(*cf, nil)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if tr.Stamp != commit {
		t.Errorf("trace Stamp = %q, want %q", tr.Stamp, commit)
	}

	// Default: stampless.
	app2 := harness.NewInProcess(t, instrumentedSUT(0), harness.WithService("loansvc"))
	cf2, err := app2.HTTP("POST", "/loan-application", nil).Capture(harness.CaptureOptions{
		Markers: happyMarkers(),
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cf2.Stamp != "" {
		t.Errorf("default capture should be stampless, got %q", cf2.Stamp)
	}
}

// TestTimingStableConcurrency is the fast-vs-slow check: lengthening one errgroup
// leg must not change the concurrent grouping (canon §3.3 rule 3).
func TestTimingStableConcurrency(t *testing.T) {
	fast := canonStructure(t, 0)
	slow := canonStructure(t, 40*time.Millisecond)
	if fast != slow {
		t.Errorf("concurrency classification changed with timing:\n--- fast ---\n%s\n--- slow ---\n%s", fast, slow)
	}
}

func canonStructure(t *testing.T, slow time.Duration) string {
	t.Helper()
	app := harness.NewInProcess(t, instrumentedSUT(slow), harness.WithService("loansvc"))
	cf, err := app.HTTP("POST", "/loan-application", nil).Capture(harness.CaptureOptions{
		Markers: happyMarkers(),
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := canon.Canonicalize(*cf, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tr.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTruncatedCaptureFailsLoudly drives a flow whose declared exit never occurs;
// the harness must report incompleteness rather than surface a partial trace.
func TestTruncatedCaptureFailsLoudly(t *testing.T) {
	app := harness.NewInProcess(t, instrumentedSUT(0), harness.WithService("loansvc"))
	cf, err := app.HTTP("POST", "/loan-application", nil).Capture(harness.CaptureOptions{
		Markers: []string{"PUBLISH nonexistent.event"}, // never published
		Quiet:   5 * time.Millisecond,
		Timeout: 150 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a truncation error")
	}
	if cf.Complete {
		t.Fatal("expected Complete=false")
	}
	// And canonicalization refuses it as the last line of defense.
	if _, cerr := canon.Canonicalize(*cf, nil); cerr != canon.ErrIncomplete {
		t.Errorf("canon error = %v, want ErrIncomplete", cerr)
	}
}

// TestEventCaptureConsumerRoot drives a consumer-rooted flow: the consumer span
// becomes the root.
func TestEventCaptureConsumerRoot(t *testing.T) {
	app := harness.NewInProcess(t, http.NewServeMux(), harness.WithService("loansvc"))
	deliver := func(ctx context.Context) {
		tr := otel.Tracer("sut")
		ctx, root := tr.Start(ctx, "consume payment.settled", oteltrace.WithSpanKind(oteltrace.SpanKindConsumer))
		root.SetAttributes(attribute.String("messaging.destination", "payment.settled"))
		_, up := tr.Start(ctx, "markPaid", oteltrace.WithSpanKind(oteltrace.SpanKindClient))
		up.SetAttributes(
			attribute.String("db.system", "postgres"),
			attribute.String("db.statement", "UPDATE loans SET status = 'paid' WHERE id = $1"),
		)
		up.End()
		root.End()
	}
	cf, err := app.Event("payment.settled", deliver).Capture(harness.CaptureOptions{
		Markers: []string{"DB postgres UPDATE loans"},
		Quiet:   10 * time.Millisecond,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if cf.Root == nil || cf.Root.Kind != ir.KindConsumer {
		t.Fatalf("root = %+v, want a consumer span", cf.Root)
	}
	tr, err := canon.Canonicalize(*cf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Root.Op != "CONSUME payment.settled" {
		t.Errorf("root op = %q", tr.Root.Op)
	}
}

func hasSpanWithAttr(spans []capture.Span, key, val string) bool {
	for i := range spans {
		if spans[i].Attrs[key] == val {
			return true
		}
	}
	return false
}

func countKind(spans []capture.Span, k ir.Kind) int {
	n := 0
	for i := range spans {
		if spans[i].Kind == k {
			n++
		}
	}
	return n
}

// TestParallelFlowsAreIsolated is the regression that justifies the shared,
// install-once provider: two flows run in parallel against the same global OTel
// pipeline and must each capture only their own spans, isolated by test.run.id —
// no cross-contamination. Run this under -race to exercise the shared recorder.
func TestParallelFlowsAreIsolated(t *testing.T) {
	run := func(service, event string) func(*testing.T) {
		return func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("POST /x", func(w http.ResponseWriter, r *http.Request) {
				_, p := otel.Tracer("svc").Start(r.Context(), "publish", oteltrace.WithSpanKind(oteltrace.SpanKindProducer))
				p.SetAttributes(attribute.String("messaging.destination.name", event))
				time.Sleep(5 * time.Millisecond) // overlap the sibling flow
				p.End()
				w.WriteHeader(http.StatusOK)
			})
			app := harness.NewInProcess(t, mux, harness.WithService(service))
			cf, err := app.HTTP("POST", "/x", nil).Capture(harness.CaptureOptions{
				Markers: []string{"PUBLISH " + event},
				Quiet:   10 * time.Millisecond,
				Timeout: 2 * time.Second,
			})
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if cf.Service != service {
				t.Errorf("service = %q, want %q", cf.Service, service)
			}
			// Every publish in this scoped flow must be THIS flow's event — the
			// sibling's spans must not leak in.
			for _, s := range cf.Spans {
				if ev := s.Attrs["messaging.destination.name"]; ev != "" && ev != event {
					t.Errorf("captured a foreign publish %q in the %q flow", ev, service)
				}
			}
		}
	}
	t.Run("A", run("svc-a", "evt.a"))
	t.Run("B", run("svc-b", "evt.b"))
}
