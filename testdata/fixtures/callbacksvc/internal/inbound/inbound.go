// Package inbound holds the consume handler dispatched by the inbox library. Its
// Handle method is registered through the manager-holds-handler idiom, so root
// discovery cannot reach it by call-resolution; it is declared in
// .flowmap.yaml entrypoints.callbacks.
package inbound

import (
	"context"

	"example.com/callbacksvc/internal/store"
)

// Handler is the inbox consume handler.
type Handler struct {
	store *store.Store
}

// New returns a Handler backed by s.
func New(s *store.Store) *Handler { return &Handler{store: s} }

// Handle persists the message — the DB INSERT that vanishes from the reachable
// graph unless this method is rooted as a declared callback.
func (h *Handler) Handle(ctx context.Context, msg string) error {
	return h.store.Insert(ctx, msg)
}
