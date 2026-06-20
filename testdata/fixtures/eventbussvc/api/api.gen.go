// Package api is a hand-written stand-in for oapi-codegen's STRICT chi-server
// output built WITH error-handler options — the
// `NewStrictHandlerWithOptions(ssi, StrictHTTPServerOptions{...})` shape. It
// reproduces the composition-root WIRING pattern the C3 rollup must classify:
//
//   - StrictHTTPServerOptions carries two error-handler hooks
//     (RequestErrorHandlerFunc / ResponseErrorHandlerFunc) the GENERATED code
//     invokes but does not define.
//   - The service's `main` supplies those hooks as CLOSURES defined in the
//     composition root and passes them in here. strictHandler stores them and
//     INVOKES them on the error path.
//
// The result is a back-edge api.strictHandler.<Op> -> <main>.newHandler$N: a real
// resolved func-value call whose body lives in `package main`. It reads backwards
// at the component altitude — main injected the closure INTO api; api does not
// depend on main — which is exactly the wiring the rollup tags `wiring` rather
// than a domain `call`.
package api

import (
	"context"
	"net/http"
)

// ---- strict request/response objects (empty stand-ins) ----

type CreateEventTypeTemplateRequestObject struct{}
type CreateEventTypeTemplateResponseObject struct{}
type SyncEventTypesRequestObject struct{}
type SyncEventTypesResponseObject struct{}
type GetHealthRequestObject struct{}
type GetHealthResponseObject struct{}

// ServerInterface is the http.HandlerFunc-shaped operation set registered on the
// router.
type ServerInterface interface {
	CreateEventTypeTemplate(w http.ResponseWriter, r *http.Request)
	SyncEventTypes(w http.ResponseWriter, r *http.Request)
	GetHealth(w http.ResponseWriter, r *http.Request)
}

// StrictServerInterface is the (ctx,request)->(response,error) operation set the
// service actually implements.
type StrictServerInterface interface {
	CreateEventTypeTemplate(ctx context.Context, request CreateEventTypeTemplateRequestObject) (CreateEventTypeTemplateResponseObject, error)
	SyncEventTypes(ctx context.Context, request SyncEventTypesRequestObject) (SyncEventTypesResponseObject, error)
	GetHealth(ctx context.Context, request GetHealthRequestObject) (GetHealthResponseObject, error)
}

// StrictHTTPServerOptions mirrors oapi-codegen's options struct: the per-request
// and per-response error-handler hooks. The generated code calls them; the
// service's composition root supplies them.
type StrictHTTPServerOptions struct {
	RequestErrorHandlerFunc  func(w http.ResponseWriter, r *http.Request, err error)
	ResponseErrorHandlerFunc func(w http.ResponseWriter, r *http.Request, err error)
}

// strictHandler adapts StrictServerInterface to ServerInterface, routing the
// error path through the INJECTED option closures.
type strictHandler struct {
	ssi     StrictServerInterface
	options StrictHTTPServerOptions
}

// NewStrictHandlerWithOptions wires the user's implementation and the
// composition root's error-handler closures into the generated handler.
func NewStrictHandlerWithOptions(ssi StrictServerInterface, options StrictHTTPServerOptions) ServerInterface {
	return &strictHandler{ssi: ssi, options: options}
}

func (sh *strictHandler) CreateEventTypeTemplate(w http.ResponseWriter, r *http.Request) {
	resp, err := sh.ssi.CreateEventTypeTemplate(r.Context(), CreateEventTypeTemplateRequestObject{})
	if err != nil {
		sh.options.ResponseErrorHandlerFunc(w, r, err) // -> injected closure (wiring back-edge)
		return
	}
	_ = resp
}

func (sh *strictHandler) SyncEventTypes(w http.ResponseWriter, r *http.Request) {
	resp, err := sh.ssi.SyncEventTypes(r.Context(), SyncEventTypesRequestObject{})
	if err != nil {
		sh.options.ResponseErrorHandlerFunc(w, r, err) // -> injected closure (wiring back-edge)
		return
	}
	_ = resp
}

func (sh *strictHandler) GetHealth(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil {
		sh.options.RequestErrorHandlerFunc(w, r, http.ErrNoLocation) // -> injected closure (wiring back-edge)
		return
	}
	resp, err := sh.ssi.GetHealth(r.Context(), GetHealthRequestObject{})
	if err != nil {
		sh.options.ResponseErrorHandlerFunc(w, r, err) // -> injected closure (wiring back-edge)
		return
	}
	_ = resp
}

// RegisterHandlers registers each operation on the stdlib ServeMux via a wrapping
// closure — the method+pattern form (Go 1.22+) flowmap's root discovery keys on.
// The wrapping closure is the HTTP root; it invokes si.<Op> through the
// ServerInterface, which RTA resolves to the strictHandler method where the
// injected closures are called.
func RegisterHandlers(mux *http.ServeMux, si ServerInterface) {
	mux.HandleFunc("POST /eventTypeTemplates", func(w http.ResponseWriter, r *http.Request) {
		si.CreateEventTypeTemplate(w, r)
	})
	mux.HandleFunc("POST /eventTypes/sync", func(w http.ResponseWriter, r *http.Request) {
		si.SyncEventTypes(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		si.GetHealth(w, r)
	})
}
