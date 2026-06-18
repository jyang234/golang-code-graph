// Package admin is an internal maintenance sub-app mounted on the custom router.
// It models two patterns common in real services that make static attribution
// hard: a FACTORY that builds handlers over injected dependencies, and a DYNAMIC
// ROUTE TABLE assembled at runtime rather than spelled out as literal
// registration calls. The handlers are named method values (not $N closures), so
// they draw no severed-closure frontier marker; mounted through the custom
// (unhinted) router, they are missed roots. PurgeLedger reaches a real DB DELETE.
package admin

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"

	"example.com/impeachsvc/internal/eventbus"
	"example.com/impeachsvc/internal/router"
	"example.com/impeachsvc/internal/store"
)

// Admin holds the dependencies the factory closes over.
type Admin struct {
	loans *store.Loans
	bus   *eventbus.Bus
}

// New builds the admin sub-app over the shared store and event bus.
func New(loans *store.Loans, bus *eventbus.Bus) *Admin { return &Admin{loans: loans, bus: bus} }

// route pairs a runtime-built pattern with its handler — the dynamic route table.
type route struct {
	method, pattern string
	handler         http.HandlerFunc
}

// table is the FACTORY: it assembles the admin routes at runtime from the
// injected store. The patterns are built here (not literals at a registration
// call site), and the handlers are method values bound to this Admin.
func (a *Admin) table() []route {
	base := "/admin"
	return []route{
		{"DELETE", base + "/ledger", a.PurgeLedger},
		{"POST", base + "/reindex", a.Reindex},
		{"POST", base + "/notify", a.Notify},
	}
}

// Mount registers every dynamically-built route on the custom router. Because
// router.Router.Add is not a recognized registrar, none of these become a
// discovered entrypoint.
func (a *Admin) Mount(r *router.Router) {
	for _, rt := range a.table() {
		r.Add(rt.method, rt.pattern, rt.handler)
	}
}

// PurgeLedger wipes the ledger — a DB DELETE reachable only through this missed
// route. This is the effect the impeachment cell must catch: statically owned by
// no discovered entrypoint, behaviorally reached on DELETE /admin/ledger.
func (a *Admin) PurgeLedger(w http.ResponseWriter, req *http.Request) {
	// Open an internal span DIRECTLY here (not via a helper) so the in-process
	// capture producer tags it with this function's runtime FQN — the L1
	// localization anchor (plan §7). PurgeLedger is reachable from no DISCOVERED
	// entrypoint (the missed admin route) yet reaches the DB DELETE, so it is the
	// precise severed node the impeachment localizes to and a blind-spot repair
	// self-extinguishes.
	ctx, span := otel.Tracer("impeachsvc").Start(req.Context(), "admin.purge")
	defer span.End()
	id := req.URL.Query().Get("loan")
	if id == "" {
		id = "ALL"
	}
	// Two named DB effects on the one missed route: the ledger and its audit trail.
	// Both are reached from no discovered entrypoint, so the missed route impeaches
	// TWO effects from a single capture — the multi-candidate witness sort the
	// single-effect corpus never exercised.
	_ = a.loans.Purge(ctx, id)
	_ = a.loans.PurgeAudit(ctx, id)
	w.WriteHeader(http.StatusNoContent)
}

// Reindex is a benign second admin route, so the missed-root pattern is not a
// single special case but a genuine sub-app surface.
func (a *Admin) Reindex(w http.ResponseWriter, req *http.Request) {
	_ = context.Canceled // no boundary effect; exercises the table's second entry
	w.WriteHeader(http.StatusAccepted)
}

// Notify publishes a constant-named event — a BUS boundary effect reached only
// through this missed route. It is the bus analogue of PurgeLedger's DB DELETE: a
// named effect static attributes to no discovered entrypoint, behaviorally observed
// on POST /admin/notify, so the impeachment cell catches it over the PUBLISH label
// vocabulary (not just DB) on a real capture.
func (a *Admin) Notify(w http.ResponseWriter, req *http.Request) {
	ctx, span := otel.Tracer("impeachsvc").Start(req.Context(), "admin.notify")
	defer span.End()
	_ = a.bus.Publish(ctx, "ledger.purged", nil)
	w.WriteHeader(http.StatusAccepted)
}
