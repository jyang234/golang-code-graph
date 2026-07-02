package harness

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// TestLinksOfMapsIdentity pins M-31: fromOTel must carry a span's OTel links (the
// cross-trace FOLLOWS_FROM continuation signal) into the internal model, not drop
// them. Only (TraceID, SpanID) identity is mapped.
func TestLinksOfMapsIdentity(t *testing.T) {
	if got := linksOf(nil); got != nil {
		t.Fatalf("linksOf(nil) = %v, want nil", got)
	}
	tid := oteltrace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	sid := oteltrace.SpanID{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8}
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: tid, SpanID: sid})
	got := linksOf([]sdktrace.Link{{SpanContext: sc}})
	if len(got) != 1 {
		t.Fatalf("linksOf mapped %d links, want 1", len(got))
	}
	if got[0].TraceID != tid.String() || got[0].SpanID != sid.String() {
		t.Errorf("link identity = (%s,%s), want (%s,%s)", got[0].TraceID, got[0].SpanID, tid.String(), sid.String())
	}
}

// flushRecorder is an http.ResponseWriter that records whether Flush was called.
type flushRecorder struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

// TestStatusRecorderUnwrapAndFlush pins M-32: the wrapper must not hide the
// underlying writer's http.Flusher. Unwrap exposes it to http.ResponseController,
// and Flush forwards directly — a streaming handler behaves under the harness as in
// production.
func TestStatusRecorderUnwrapAndFlush(t *testing.T) {
	inner := &flushRecorder{ResponseWriter: httptest.NewRecorder()}
	rw := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}

	if rw.Unwrap() != inner {
		t.Error("Unwrap must return the wrapped ResponseWriter so ResponseController can reach it")
	}
	// http.ResponseController.Flush walks Unwrap() until it finds a Flusher.
	if err := http.NewResponseController(rw).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush through the wrapper: %v", err)
	}
	if !inner.flushed {
		t.Error("Flush did not reach the underlying writer — a streaming handler would be silently buffered")
	}
}
