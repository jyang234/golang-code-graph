// Package bus is a minimal event-bus publisher classified as a bus-publish boundary
// (see .flowmap.yaml). The topic is the first string argument, so the labeler reads
// it as the PUBLISH target.
package bus

import "context"

// Bus publishes events.
type Bus struct{}

// New returns a Bus.
func New() *Bus { return &Bus{} }

// Publish sends payload to topic. Never executed under static analysis.
func (b *Bus) Publish(ctx context.Context, topic string, payload []byte) error {
	_, _, _ = ctx, topic, payload
	return nil
}
