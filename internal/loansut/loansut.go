// Package loansut is the shared system-under-test for flowmap's behavioral
// tests: a miniature, OTel-instrumented loan service that both the harness suite
// and the public flow-DSL suite drive through the real router. Keeping one
// instrumented handler — rather than a near-identical copy per package — means
// the canonical shape the goldens assert against has a single source of truth,
// so a change to the modeled flow can't silently diverge between the two suites.
//
// The handler shape is fixed (the ops, attributes, and span kinds the
// canonicalizer reads); Options vary only what the goldens are insensitive to —
// timings, the tracer name, and an optional injected publish for drift tests —
// plus the slow-leg knob that proves concurrency classification is timing-stable.
package loansut

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Options configure the loan SUT. The zero value is the canonical service: the
// shape the committed flow golden asserts against. Every field varies only what
// the golden is insensitive to, so adjusting them never rebases the snapshot.
type Options struct {
	// Tracer is the OTel tracer name. Defaults to "loansvc". The canonical shape
	// is independent of it.
	Tracer string

	// LegSleep is how long each concurrent evaluation leg performs I/O. Defaults
	// to 8ms. It exists so both legs' spans reliably overlap in wall-clock time,
	// the interval-overlap fallback for the structural concurrency signal.
	LegSleep time.Duration

	// SlowLeg lengthens the credit-bureau leg on top of LegSleep. The
	// timing-stability test drives the same flow fast and slow and asserts the
	// concurrent grouping is unchanged.
	SlowLeg time.Duration

	// AuditDelay is how long the fire-and-forget audit write waits before
	// starting its span, after the response is on its way. Defaults to 3ms. It
	// forces the harness to drain a span that begins post-response.
	AuditDelay time.Duration

	// ExtraPublishes are additional events published after loan.approved. They
	// inject behavioral drift (a new contract op) for the gate-fails-on-drift
	// test; the canonical SUT passes none.
	ExtraPublishes []string
}

func (o Options) tracer() string {
	if o.Tracer == "" {
		return "loansvc"
	}
	return o.Tracer
}

func (o Options) legSleep() time.Duration {
	if o.LegSleep <= 0 {
		return 8 * time.Millisecond
	}
	return o.LegSleep
}

func (o Options) auditDelay() time.Duration {
	if o.AuditDelay <= 0 {
		return 3 * time.Millisecond
	}
	return o.AuditDelay
}

// Handler builds the loan-application router. The applicant read and the
// credit-score call are dispatched onto separate goroutines and both perform
// real I/O, so they race (the canonicalizer reads them as a concurrent pair);
// then a sequential charge → publish → ledger, and a fire-and-forget audit write
// that begins after the response.
func Handler(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /loan-application", func(w http.ResponseWriter, r *http.Request) {
		tr := otel.Tracer(opts.tracer())
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
			time.Sleep(opts.legSleep())
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
			time.Sleep(opts.legSleep() + opts.SlowLeg)
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

		for _, event := range opts.ExtraPublishes {
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

		// Fire-and-forget audit: starts a span after the response is on its way,
		// so the harness must drain it before declaring completeness.
		auditCtx := context.WithoutCancel(ctx)
		delay := opts.auditDelay()
		go func() {
			time.Sleep(delay)
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
