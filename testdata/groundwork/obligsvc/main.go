// obligsvc is groundwork's path-obligations fixture: every app function is
// reachable from main so the call graph (and therefore the obligations check)
// covers each verdict shape. The HTTP route exists so the fixture has a NAMED
// entrypoint — entry-scoped builds must omit the obligations section, and that
// needs an entry to scope to.
package main

import (
	"net/http"

	"example.com/obligsvc/internal/app"
	"example.com/obligsvc/internal/store"
)

func main() {
	s := &store.Store{}
	_ = app.Transfer(s)
	_ = app.TransferDefer(s)
	tx, _ := app.TransferOwn(s)
	if tx != nil {
		_ = tx.Commit()
	}
	app.Disburse(true)
	app.DisburseRacy(true)
	_ = app.TransferRecoverNamed(s)
	_ = app.TransferClosure(s)
	_ = app.TransferAnnotate(s)
	_ = app.TransferConcrete(s)
	_ = app.HoldSem(s)
	app.DeferredPublish()
	app.DeferredPublishAudited()
	_ = app.DisburseAndCharge("id")
	_ = app.DisburseAndChargeRisky("id", true)

	mux := http.NewServeMux()
	mux.HandleFunc("/transfer", func(w http.ResponseWriter, r *http.Request) {
		_ = app.Transfer(&store.Store{})
	})
	_ = http.ListenAndServe(":0", mux)
}
