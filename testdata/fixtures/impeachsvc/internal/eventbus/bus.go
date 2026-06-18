// Package eventbus is the fixture's stand-in for an internal message bus, named
// under .flowmap.yaml's classify.busPublish hint so the static extractor treats
// (*Bus).Publish as an outbound-async boundary effect. It exists so the missed
// admin route can reach a BUS effect, not only DB effects — exercising the
// impeachment cell's bus vocabulary (the label rung's PUBLISH key) on a real
// capture, the direction the DB-only corpus never drove.
package eventbus

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// tracer is fetched per span (never cached at package init): the OTel global binds
// a cached delegating tracer to the first provider installed, which would route a
// second in-process test's spans to the first test's recorder. Fetching per span
// always resolves the current provider.
func tracer() trace.Tracer { return otel.Tracer("impeachsvc") }

// Bus is a trivial in-process publish sink; the fixture exists to be analyzed and
// captured, not to be a real broker.
type Bus struct{}

// New returns a Bus.
func New() *Bus { return &Bus{} }

// Publish emits one event. The event name is recorded as the published-event
// contract; a CONSTANT name (as the missed route passes) keeps the effect statically
// NAMED (boundary:bus publish <event>), the impeachment precondition — a non-constant
// name would instead become a NonConstantBoundaryArg blind spot.
func (b *Bus) Publish(ctx context.Context, event string, payload []byte) error {
	_, span := tracer().Start(ctx, "bus.publish", trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()
	span.SetAttributes(attribute.String("messaging.destination.name", event))
	return nil
}
