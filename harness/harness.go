// Package harness is flowmap's PUBLIC in-process capture harness: a target
// service repository imports it to drive one flow through its real router (or
// consumer path) under a fully-instrumented OpenTelemetry pipeline and hand the
// captured trace to canonicalization (trace-capture-harness spec). It wires the
// real OTel SDK with an in-memory recorder, AlwaysSample, and a baggage→attribute
// processor so every span is attributable to exactly one test run, then waits for
// quiescence and refuses to surface a truncated trace.
//
// This package is part of the stable public surface (plan [C1]); its exported
// types are the consumer contract. Internally it adapts real OTel spans into
// flowmap's OTel-free internal span model (decision D8), so canonicalization and
// everything downstream never import OTel.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelbaggage "go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/jyang234/golang-code-graph/internal/await"
	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// TB is the subset of *testing.T the harness needs, so the package does not force
// a testing import on non-test consumers and stays mockable.
type TB interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

// App is an installed in-process harness: a tracer provider wired to an in-memory
// recorder, plus the service's real HTTP handler. Construct it with NewInProcess
// and drive it with HTTP or Event.
type App struct {
	t       TB
	handler http.Handler
	rec     *tracetest.SpanRecorder
	tp      *sdktrace.TracerProvider
	prop    propagation.TextMapPropagator
	service string
}

type options struct {
	service string
}

// Option configures NewInProcess.
type Option func(*options)

// WithService sets the self-lifeline service name recorded as the OTel resource
// service.name and stamped onto the canonical trace.
func WithService(name string) Option { return func(o *options) { o.service = name } }

// NewInProcess installs an in-process OTel pipeline for the duration of the test
// and returns a harness over handler (the service's real router). It sets the
// global tracer provider and propagator so the instrumented service-under-test —
// which obtains its tracer from otel.Tracer — emits into the in-memory recorder,
// and restores the previous globals on cleanup.
func NewInProcess(t TB, handler http.Handler, opts ...Option) *App {
	t.Helper()
	o := options{service: "service"}
	for _, fn := range opts {
		fn(&o)
	}

	rec := tracetest.NewSpanRecorder()
	res, _ := resource.New(context.Background(),
		resource.WithAttributes(attribute.String("service.name", o.service)))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(startProcessor{}),
		sdktrace.WithSpanProcessor(rec),
	)
	prop := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})

	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	return &App{t: t, handler: handler, rec: rec, tp: tp, prop: prop, service: o.service}
}

// goroutineAttr is the span attribute the start processor stamps with the
// goroutine the span began on — flowmap's structural concurrency signal
// (canon §3.3 / plan [C2]). It is consumed into capture.Span.Goroutine and never
// reaches the canonical snapshot.
const goroutineAttr = "flowmap.goid"

// startProcessor runs synchronously on the goroutine that starts each span. It
// copies the test.run.id baggage member onto the span (so spans are queryable by
// the correlation key even when instrumentation begins a fresh trace on entry,
// harness §3) and records the starting goroutine's id. Baggage is not
// automatically a span attribute, which is why this small processor exists.
type startProcessor struct{}

func (startProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	if m := otelbaggage.FromContext(parent).Member(capture.CorrelationKey); m.Value() != "" {
		s.SetAttributes(attribute.String(capture.CorrelationKey, m.Value()))
	}
	s.SetAttributes(attribute.Int64(goroutineAttr, int64(goid())))
}
func (startProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (startProcessor) Shutdown(context.Context) error   { return nil }
func (startProcessor) ForceFlush(context.Context) error { return nil }

// goid returns the current goroutine's id by parsing the runtime stack header
// ("goroutine N [state]:"). There is no public API for this; it is confined to
// this test-harness boundary and used only as the structural concurrency signal.
func goid() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := bytes.Fields(buf[:n])
	if len(fields) < 2 {
		return 0
	}
	id, err := strconv.ParseUint(string(fields[1]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// Pending is a triggered-but-not-yet-collected flow. Call Capture to await
// quiescence and produce the scoped CapturedFlow.
type Pending struct {
	app     *App
	runID   string
	flow    string
	trigger capture.TriggerKind
}

// HTTP drives method+target (with an optional body) through the real router as a
// server-rooted flow. It mints a controlled trace id and a unique test.run.id,
// injects both via W3C propagation, and records the service's handling under a
// server span whose http.route is the templated path.
func (a *App) HTTP(method, target string, body []byte) *Pending {
	a.t.Helper()
	runID := a.newRunID()
	ctx := a.injectedContext(runID)

	req := httptest.NewRequest(method, target, bytesReader(body)).WithContext(context.Background())
	a.prop.Inject(ctx, propagation.HeaderCarrier(req.Header))

	srv := a.serverSpanMiddleware(a.handler)
	srv.ServeHTTP(httptest.NewRecorder(), req)

	return &Pending{app: a, runID: runID, flow: method + " " + templatePath(target), trigger: capture.TriggerHTTP}
}

// Event drives a consumer-rooted flow. It builds a context carrying the trace
// context and the test.run.id and invokes deliver, which runs the service's real
// consumer path; that consumer's span becomes the flow root.
func (a *App) Event(name string, deliver func(ctx context.Context)) *Pending {
	a.t.Helper()
	runID := a.newRunID()
	ctx := a.injectedContext(runID)
	// Round-trip through the propagator so the consumer path sees the same
	// extract-from-headers shape a real broker delivery would.
	carrier := propagation.MapCarrier{}
	a.prop.Inject(ctx, carrier)
	deliverCtx := a.prop.Extract(context.Background(), carrier)
	deliver(deliverCtx)
	return &Pending{app: a, runID: runID, flow: "consume " + name, trigger: capture.TriggerEvent}
}

// CaptureOptions tune quiescence detection. Zero values fall back to spec
// defaults (2s quiet, 5s timeout).
type CaptureOptions struct {
	Markers  []string      // declared expected-exit op keys
	Quiet    time.Duration // idle interval required after the last span
	Timeout  time.Duration // hard deadline before failing loudly
	MinSpans int           // sanity floor on span count
}

// Capture awaits quiescence and returns the scoped, complete CapturedFlow. It
// returns an error (truncated capture) when the markers are not all observed
// before the deadline — the caller must not snapshot it.
func (p *Pending) Capture(opt CaptureOptions) (*capture.CapturedFlow, error) {
	a := p.app
	a.t.Helper()
	if opt.Quiet == 0 {
		opt.Quiet = 2 * time.Second
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Second
	}

	snapshot := func() []capture.Span {
		spans := a.collect()
		scoped, _ := capture.Scope(spans, p.runID)
		return scoped
	}
	spans, complete := await.Await(snapshot, await.Options{
		Markers:  opt.Markers,
		Match:    markerMatch,
		Quiet:    opt.Quiet,
		Timeout:  opt.Timeout,
		MinSpans: opt.MinSpans,
		Poll:     time.Millisecond,
	})

	scoped, root := capture.Scope(spans, p.runID)
	cf := &capture.CapturedFlow{
		Flow:     p.flow,
		Service:  a.service,
		Trigger:  p.trigger,
		Mode:     capture.ModeInProcess,
		Spans:    scoped,
		Root:     root,
		Complete: complete && root != nil,
	}
	if !cf.Complete {
		return cf, errTruncated{flow: p.flow}
	}
	return cf, nil
}

type errTruncated struct{ flow string }

func (e errTruncated) Error() string {
	return "harness: flow " + e.flow + " did not reach quiescence (truncated capture); refusing to snapshot"
}

// markerMatch reports whether a span's canonical op key equals a declared
// expected-exit marker, coupling the marker grammar to canonical op keys.
func markerMatch(s capture.Span, marker string) bool {
	op, _ := opkey.Of(s.Kind, s.Attrs, s.Name)
	return op == marker
}

// collect adapts the recorder's finished OTel spans into flowmap's internal span
// model. This is the single boundary where OTel types are read; nothing
// downstream sees them.
func (a *App) collect() []capture.Span {
	ended := a.rec.Ended()
	out := make([]capture.Span, 0, len(ended))
	for _, s := range ended {
		out = append(out, fromOTel(s))
	}
	return out
}

func (a *App) newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	const hex = "0123456789abcdef"
	var sb [16]byte
	for i, x := range b {
		sb[i*2] = hex[x>>4]
		sb[i*2+1] = hex[x&0xf]
	}
	return string(sb[:])
}

// injectedContext returns a context carrying a controlled, sampled trace id and
// the test.run.id baggage member — the two correlation keys (harness §3).
func (a *App) injectedContext(runID string) context.Context {
	var tid oteltrace.TraceID
	var sid oteltrace.SpanID
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	ctx := oteltrace.ContextWithSpanContext(context.Background(), sc)
	m, _ := otelbaggage.NewMember(capture.CorrelationKey, runID)
	bag, _ := otelbaggage.New(m)
	return otelbaggage.ContextWithBaggage(ctx, bag)
}

// serverSpanMiddleware represents the router's instrumentation: it extracts the
// incoming trace context, opens the server span that becomes the flow root, sets
// the HTTP semantic-convention attributes, and records the response status.
func (a *App) serverSpanMiddleware(next http.Handler) http.Handler {
	tracer := a.tp.Tracer("flowmap/harness")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := a.prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := templatePath(r.URL.Path)
		ctx, span := tracer.Start(ctx, r.Method+" "+route, oteltrace.WithSpanKind(oteltrace.SpanKindServer))
		span.SetAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
		)
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))
		span.SetAttributes(attribute.String("http.response.status_code", strconv.Itoa(rw.status)))
		if rw.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rw.status))
		}
		span.End()
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// fromOTel maps one finished OTel span into the internal model.
func fromOTel(s sdktrace.ReadOnlySpan) capture.Span {
	attrs := map[string]string{}
	var goroutine uint64
	for _, kv := range s.Attributes() {
		if string(kv.Key) == goroutineAttr {
			goroutine = uint64(kv.Value.AsInt64())
			continue // structural signal, not part of the canonical attributes
		}
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	cs := capture.Span{
		ID:        s.SpanContext().SpanID().String(),
		ParentID:  s.Parent().SpanID().String(),
		Name:      s.Name(),
		Kind:      kindOf(s.SpanKind()),
		Attrs:     attrs,
		Start:     s.StartTime(),
		End:       s.EndTime(),
		Goroutine: goroutine,
	}
	switch s.Status().Code {
	case codes.Ok:
		cs.Status = capture.StatusOK
	case codes.Error:
		cs.Status = capture.StatusError
		if et := attrs["error.type"]; et != "" {
			cs.ErrorType = et
		} else {
			cs.ErrorType = "error"
		}
	default:
		cs.Status = capture.StatusUnset
	}
	return cs
}

func kindOf(k oteltrace.SpanKind) ir.Kind {
	switch k {
	case oteltrace.SpanKindServer:
		return ir.KindServer
	case oteltrace.SpanKindClient:
		return ir.KindClient
	case oteltrace.SpanKindProducer:
		return ir.KindProducer
	case oteltrace.SpanKindConsumer:
		return ir.KindConsumer
	default:
		return ir.KindInternal
	}
}
