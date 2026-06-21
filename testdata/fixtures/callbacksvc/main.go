// Command callbacksvc reproduces the two entry-point classes call-resolution
// cannot reach: a library-dispatched callback (inbound.Handler.Handle, registered
// through the manager-holds-handler idiom) and a background goroutine worker
// (reconciler.Reconciler.Start, launched with `go`). Both are declared in
// .flowmap.yaml so root discovery roots them directly; without the declarations the
// callback's DB INSERT is orphaned and the worker's DB read is attributed to main.
package main

import (
	"example.com/callbacksvc/internal/inbound"
	"example.com/callbacksvc/internal/reconciler"
	"example.com/callbacksvc/internal/store"
	"example.com/callbacksvc/internal/subscriptions"
)

func main() {
	st := store.New(nil)

	// The callback: its handler value threads through Config → constructor → field,
	// so the inbox registration cannot be resolved back to inbound.Handler.Handle.
	h := inbound.New(st)
	m := subscriptions.New(subscriptions.Config{Handler: h.Handle})
	m.Start()

	// The worker: reachable via this `go`, but not an entry point.
	rec := reconciler.New(st)
	go rec.Start()
}
