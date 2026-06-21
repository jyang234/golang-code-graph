// Package reconciler is a background goroutine worker. main launches Start with
// `go`, so it IS reachable — this is not a soundness hole. But it is not an entry
// point, so without a declaration its DB read is attributed to main rather than to
// a worker route and cannot be gated per-worker. .flowmap.yaml
// entrypoints.workers roots it as its own entry.
package reconciler

import (
	"context"

	"example.com/callbacksvc/internal/store"
)

// Reconciler periodically scans for pending work.
type Reconciler struct {
	store *store.Store
}

// New returns a Reconciler backed by s.
func New(s *store.Store) *Reconciler { return &Reconciler{store: s} }

// Start runs the reconcile loop. Launched with `go` from main.
func (r *Reconciler) Start() {
	for {
		r.reconcileOnce()
	}
}

// reconcileOnce performs one scan — the worker's recovered effect.
func (r *Reconciler) reconcileOnce() {
	_ = r.store.Scan(context.Background())
}
