// Command topicfoldsvc is the reclaim-topic fixture. main is a root, so each Publish
// it reaches appears as a bus boundary edge. It publishes three topic shapes the
// labeler must bin: a compile-time constant (resolved without the fold), a Phi of two
// constants (recovered only under --reclaim-topic, fanned into one edge per topic),
// and a genuinely dynamic topic (stays <dynamic> even with the fold).
package main

import (
	"context"
	"os"

	"example.com/topicfoldsvc/bus"
)

func main() {
	b := bus.New()
	ctx := context.Background()

	_ = b.Publish(ctx, "orders.created", nil) // constant — resolved without the fold

	publishOrder(b, ctx, len(os.Args) > 1) // Phi of two constants — reclaim-topic recovers both

	_ = b.Publish(ctx, os.Getenv("TOPIC"), nil) // genuinely dynamic — stays <dynamic>
}

// publishOrder selects the topic at runtime from a finite, provably-complete set of
// two constants — the const-set the reclaim-topic fold proves and names.
func publishOrder(b *bus.Bus, ctx context.Context, cancelled bool) {
	topic := "orders.shipped"
	if cancelled {
		topic = "orders.cancelled"
	}
	_ = b.Publish(ctx, topic, nil)
}
