// Command eventbussvc is a toy service whose HTTP routes are registered the way
// oapi-codegen's STRICT chi server WITH OPTIONS does:
// NewStrictHandlerWithOptions wraps the implementation and takes per-request /
// per-response error-handler hooks. Those hooks are CLOSURES defined HERE, in the
// composition root, and injected into the generated `api` handler — so the static
// graph carries a back-edge api.strictHandler.<Op> -> eventbussvc.newHandler$N.
//
// That back-edge is the composition-root WIRING the C3 rollup classifies: a real
// resolved func-value call whose body lives in `package main`, which reads
// backwards at the component altitude (main injected the closure into api; api
// does not depend on main).
package main

import (
	"net/http"

	"example.com/eventbussvc/api"
	"example.com/eventbussvc/server"
	"example.com/eventbussvc/store"
)

// newHandler assembles the strict handler, injecting the composition root's
// error-handler closures. The two anonymous funcs are eventbussvc.newHandler$1
// and $2 — the injected wiring the api package later invokes.
func newHandler(srv api.StrictServerInterface) api.ServerInterface {
	return api.NewStrictHandlerWithOptions(srv, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		},
	})
}

func main() {
	mux := http.NewServeMux()
	srv := server.New(store.New(nil))
	api.RegisterHandlers(mux, newHandler(srv))
	_ = http.ListenAndServe(":8080", mux)
}
