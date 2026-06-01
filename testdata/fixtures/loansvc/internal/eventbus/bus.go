// Package eventbus is the fixture's stand-in for an internal message bus. It is
// named under .flowmap.yaml's classify.busPublish / classify.busConsume hints, so
// the static extractor treats (*Bus).Publish as an outbound-async boundary effect
// and (*Bus).Subscribe as a consumer registrar whose handler argument is a
// synthetic root.
package eventbus

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// tracer is fetched per span (never cached at package init): the OTel
// global binds a cached delegating tracer to the first provider installed,
// which would route a second in-process test's spans to the first test's
// recorder. Fetching per span always resolves the current provider.
func tracer() trace.Tracer { return otel.Tracer("loansvc") }

// Handler consumes one event payload. It is the func-value shape the static
// extractor resolves to a consumer root.
type Handler func(ctx context.Context, payload []byte) error

// Bus is an in-process publish/subscribe fan-out. It is deliberately trivial; the
// fixture exists to be analyzed statically, not to be a real broker.
type Bus struct {
	subs map[string][]Handler
}

// New returns an empty Bus.
func New() *Bus { return &Bus{subs: make(map[string][]Handler)} }

// Publish delivers payload to every subscriber of event. The event name (the
// second argument) is what the boundary contract records as a published event;
// when a caller passes a non-constant name, that publish becomes a
// NonConstantBoundaryArg blind spot.
func (b *Bus) Publish(ctx context.Context, event string, payload []byte) error {
	ctx, span := tracer().Start(ctx, "bus.publish", trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()
	span.SetAttributes(attribute.String("messaging.destination.name", event))
	for _, h := range b.subs[event] {
		if err := h(ctx, payload); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe registers h as a consumer of event. The static extractor reads the
// event-name argument as the consumed-event contract and the handler argument as
// a synthetic consumer root.
func (b *Bus) Subscribe(event string, h Handler) {
	b.subs[event] = append(b.subs[event], h)
}
