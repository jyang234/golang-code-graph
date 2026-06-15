// Command strictsvc is a toy service whose HTTP routes are registered the way
// oapi-codegen's STRICT chi server does — NewStrictHandler wraps the
// implementation, HandlerWithOptions registers the per-operation wrappers on a
// chi router. The handler chain for each route therefore runs through the
// generated `$1` closures and the http.Handler dispatch seam, exercising the
// strict-server topology R3/R7/R8 are about.
package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"example.com/strictsvc/api"
	"example.com/strictsvc/server"
	"example.com/strictsvc/store"
)

func main() {
	r := chi.NewRouter()
	srv := server.New(store.New(nil))
	api.HandlerWithOptions(api.NewStrictHandler(srv), r, "")
	_ = http.ListenAndServe(":8080", r)
}
