// Command loansvc-otlp is a runnable, OTLP-exporting stand-in for a real
// instrumented service — the worked reference for wiring a service into
// flowmap's post-hoc path (docs/integration). It emits the loansvc boundary
// shape (an inbound entry, two outbound HTTP deps, a publish, two DB writes) and
// shows the three things an adopting service must get right:
//
//  1. an OTLP exporter to the collector (AlwaysSample),
//  2. W3C trace-context + baggage propagation on the inbound request,
//  3. a baggagecopy span processor that promotes the flowmap.flow baggage member
//     onto every span as an attribute — without it, the collector's tail-sampling
//     and flowmap's grouping (both keyed on span attributes) never see the tag.
//
// It is a standalone module so its OTel SDK / contrib dependencies stay out of
// flowmap's engine graph.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/processors/baggagecopy"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// flowmapMembers is the baggagecopy filter: promote only flowmap's correlation
// members onto spans, not the entire baggage (which could carry unrelated keys).
func flowmapMembers(m baggage.Member) bool {
	return m.Key() == "flowmap.flow" || m.Key() == "Correlation-Id"
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	// (1) OTLP/HTTP exporter — endpoint from OTEL_EXPORTER_OTLP_ENDPOINT
	// (default localhost:4318). Insecure for the local/demo collector.
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithInsecure())
	if err != nil {
		return err
	}
	res := resource.NewSchemaless(attribute.String("service.name", "loansvc"))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // flowmap-tagged flows are 100% sampled
		sdktrace.WithResource(res),
		// (3) the load-bearing step: baggage is not in exported spans, so promote
		// flowmap.flow onto every span at start.
		sdktrace.WithSpanProcessor(baggagecopy.NewSpanProcessor(flowmapMembers)),
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(500*time.Millisecond)),
	)
	otel.SetTracerProvider(tp)
	// (2) propagate trace context AND baggage, so the flowmap.flow tag the caller
	// sets survives the hop.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	mux := http.NewServeMux()
	// The mux pattern carries the method; http.route is just the path template.
	mux.Handle("POST /loan-application", entry("/loan-application", handleLoanApplication))

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println("server:", err)
		}
	}()
	log.Println("loansvc-otlp listening on :8080")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return tp.Shutdown(shutCtx) // flushes the batcher
}

// entry is the server-span middleware: extract the inbound trace context +
// baggage, open the flow's root server span, record status. This is the seam an
// otelhttp instrumentation gives you for free.
func entry(route string, h func(context.Context, http.ResponseWriter, *http.Request)) http.Handler {
	tr := otel.Tracer("loansvc")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tr.Start(ctx, r.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
		)
		h(ctx, w, r)
		span.SetAttributes(attribute.Int("http.response.status_code", http.StatusOK))
		span.SetStatus(codes.Ok, "")
	})
}

// handleLoanApplication emits the loansvc boundary shape: a concurrent
// credit-score read, an outbound charge, the approval publish, and two DB writes.
func handleLoanApplication(ctx context.Context, w http.ResponseWriter, _ *http.Request) {
	tr := otel.Tracer("loansvc")

	_, score := tr.Start(ctx, "GET /score", trace.WithSpanKind(trace.SpanKindClient))
	score.SetAttributes(
		attribute.String("http.request.method", "GET"),
		attribute.String("peer.service", "credit-bureau"),
		attribute.String("http.target", "/score/8412"),
	)
	score.End()

	_, charge := tr.Start(ctx, "POST /charge", trace.WithSpanKind(trace.SpanKindClient))
	charge.SetAttributes(
		attribute.String("http.request.method", "POST"),
		attribute.String("peer.service", "payment-gw"),
		attribute.String("http.target", "/charge/8412"),
	)
	charge.End()

	_, pub := tr.Start(ctx, "loan.approved publish", trace.WithSpanKind(trace.SpanKindProducer))
	pub.SetAttributes(attribute.String("messaging.destination.name", "loan.approved"))
	pub.End()

	_, ledger := tr.Start(ctx, "ledger insert", trace.WithSpanKind(trace.SpanKindClient))
	ledger.SetAttributes(
		attribute.String("db.system", "postgres"),
		attribute.String("db.statement", "INSERT INTO ledger (loan_id, amount) VALUES ($1, $2)"),
	)
	ledger.End()

	_, audit := tr.Start(ctx, "audit insert", trace.WithSpanKind(trace.SpanKindClient))
	audit.SetAttributes(
		attribute.String("db.system", "postgres"),
		attribute.String("db.statement", "INSERT INTO audit_log (loan_id) VALUES ($1)"),
	)
	audit.End()

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"approved"}`))
}
