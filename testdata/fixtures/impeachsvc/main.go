// Command impeachsvc is a fixture whose purpose is to make the static analyzer's
// own blind spot CONCRETE and behaviorally catchable. It wires two route
// surfaces:
//
//   - a PUBLIC route registered through net/http's recognized HandleFunc
//     (POST /loan -> handler.App.Create -> DB INSERT loans): a discovered
//     entrypoint whose effect is soundly attributed;
//   - an ADMIN sub-app mounted through a bespoke custom router via a factory +
//     dynamic route table (DELETE /admin/ledger -> admin.Admin.PurgeLedger ->
//     DB DELETE ledger): a MISSED ROOT. The builder still reaches PurgeLedger
//     from main (the handler value is address-taken in the router's map), so the
//     DELETE effect is in the graph — but no discovered entrypoint owns it and
//     nothing discloses the gap.
//
// Driven behaviorally, the DELETE is observed on a path the static graph cannot
// attribute to any route: a genuine impeachment of the analyzer's route→effect
// completeness, caught with a runtime witness.
package main

import (
	"log"
	"net/http"

	"example.com/impeachsvc/internal/admin"
	"example.com/impeachsvc/internal/eventbus"
	"example.com/impeachsvc/internal/handler"
	"example.com/impeachsvc/internal/router"
	"example.com/impeachsvc/internal/store"
)

func main() { log.Fatal(run()) }

func run() error {
	loans := store.New(nil)
	bus := eventbus.New()
	app := handler.New(loans)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /loan", app.Create) // recognized registrar -> discovered entrypoint

	// The admin sub-app is mounted through the custom router; mux.Handle takes an
	// http.Handler (not func-typed) and is not a recognized registrar, so neither
	// the mount nor the routes inside it are discovered.
	adminRouter := router.New()
	admin.New(loans, bus).Mount(adminRouter)
	mux.Handle("/admin/", adminRouter)

	httpSrv := &http.Server{Addr: ":8080", Handler: mux}
	return httpSrv.ListenAndServe()
}
