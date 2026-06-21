// Package inbox is a stand-in for a library whose DISPATCH is out-of-module. It
// stores the registered handler and never invokes it through any in-scope edge, so
// root discovery's registrar resolver — which runs before the call graph and has no
// points-to to lean on — cannot recover the handler's effect cone from the
// registration call. The .flowmap.yaml entrypoints.callbacks declaration is what
// roots it.
package inbox

import "context"

// Handler is a registered consume handler.
type Handler func(ctx context.Context, msg string) error

// Inbox holds a registered handler. A real inbox dispatches it from another
// module's goroutine; here it is never called, which is the point.
type Inbox struct {
	h Handler
}

// New returns an empty inbox.
func New() *Inbox { return &Inbox{} }

// Register stores h. h reaches this call as a struct-field load at the call site
// (see subscriptions.Manager.Start), which the resolver cannot trace to a concrete
// function — the value-flow wall.
func Register(ib *Inbox, h Handler) { ib.h = h }
