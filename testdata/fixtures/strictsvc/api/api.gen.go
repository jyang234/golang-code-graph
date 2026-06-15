// Package api is a hand-written stand-in for oapi-codegen's STRICT chi-server
// output (`strict-server: true`), the shape field report R3/R7/R8 say no fixture
// in the matrix has. Three layers, exactly as the generator emits them:
//
//  1. ServerInterface — http.HandlerFunc-shaped operations chi registers.
//  2. ServerInterfaceWrapper — adapts each operation to an http.Handler by
//     building a PER-HANDLER closure (the `$1`), wrapping it through the
//     configured middleware, then dispatching via the http.Handler INTERFACE.
//     That interface hop is the dynamic seam: the wrapper method is an HTTP root
//     (chi registers it), but its static out-edges stop at the dispatch — they do
//     not cross into its own `$1` closure, where the real handler chain lives.
//  3. strictHandler — adapts the user's StrictServerInterface (ctx,request)->
//     (response,error) to ServerInterface, again via a per-op inner closure.
//
// The classified DB write that R7 is about sits past the seam:
// wrapper.CreateEventTypeTemplate (root) ──seam──> $1 ──> strictHandler ──>
// server.Server ──> store DELETE.
package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// ---- strict request/response objects (empty stand-ins) ----

type CreateEventTypeTemplateRequestObject struct{}
type CreateEventTypeTemplateResponseObject struct{}
type SyncEventTypesRequestObject struct{}
type SyncEventTypesResponseObject struct{}
type GetHealthRequestObject struct{}
type GetHealthResponseObject struct{}

// ServerInterface is the http.HandlerFunc-shaped operation set chi registers.
type ServerInterface interface {
	CreateEventTypeTemplate(w http.ResponseWriter, r *http.Request)
	SyncEventTypes(w http.ResponseWriter, r *http.Request)
	GetHealth(w http.ResponseWriter, r *http.Request)
}

// MiddlewareFunc matches oapi-codegen's per-operation middleware hook.
type MiddlewareFunc func(http.Handler) http.Handler

// ServerInterfaceWrapper adapts ServerInterface to http.Handler with middleware.
type ServerInterfaceWrapper struct {
	Handler            ServerInterface
	HandlerMiddlewares []MiddlewareFunc
}

func (siw *ServerInterfaceWrapper) CreateEventTypeTemplate(w http.ResponseWriter, r *http.Request) {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siw.Handler.CreateEventTypeTemplate(w, r)
	})
	for _, middleware := range siw.HandlerMiddlewares {
		handler = middleware(handler)
	}
	handler.ServeHTTP(w, r)
}

func (siw *ServerInterfaceWrapper) SyncEventTypes(w http.ResponseWriter, r *http.Request) {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siw.Handler.SyncEventTypes(w, r)
	})
	for _, middleware := range siw.HandlerMiddlewares {
		handler = middleware(handler)
	}
	handler.ServeHTTP(w, r)
}

func (siw *ServerInterfaceWrapper) GetHealth(w http.ResponseWriter, r *http.Request) {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siw.Handler.GetHealth(w, r)
	})
	for _, middleware := range siw.HandlerMiddlewares {
		handler = middleware(handler)
	}
	handler.ServeHTTP(w, r)
}

// HandlerWithOptions registers the wrapper methods on a chi router under baseURL,
// the same r.Post/r.Get-inside-r.Group shape oapi-codegen emits (and the shape
// flowmap's root discovery keys on).
func HandlerWithOptions(si ServerInterface, r chi.Router, baseURL string) http.Handler {
	wrapper := ServerInterfaceWrapper{Handler: si}
	r.Group(func(r chi.Router) {
		r.Post(baseURL+"/eventTypeTemplates", wrapper.CreateEventTypeTemplate)
		r.Post(baseURL+"/eventTypes/sync", wrapper.SyncEventTypes)
		r.Get(baseURL+"/healthz", wrapper.GetHealth)
	})
	return r
}

// StrictServerInterface is the (ctx,request)->(response,error) operation set the
// service actually implements.
type StrictServerInterface interface {
	CreateEventTypeTemplate(ctx context.Context, request CreateEventTypeTemplateRequestObject) (CreateEventTypeTemplateResponseObject, error)
	SyncEventTypes(ctx context.Context, request SyncEventTypesRequestObject) (SyncEventTypesResponseObject, error)
	GetHealth(ctx context.Context, request GetHealthRequestObject) (GetHealthResponseObject, error)
}

// strictHandler adapts StrictServerInterface to ServerInterface.
type strictHandler struct {
	ssi StrictServerInterface
}

// NewStrictHandler wires the user's implementation into the generated wrapper.
func NewStrictHandler(ssi StrictServerInterface) ServerInterface {
	return &strictHandler{ssi: ssi}
}

func (sh *strictHandler) CreateEventTypeTemplate(w http.ResponseWriter, r *http.Request) {
	handler := func(ctx context.Context) (CreateEventTypeTemplateResponseObject, error) {
		return sh.ssi.CreateEventTypeTemplate(ctx, CreateEventTypeTemplateRequestObject{})
	}
	if _, err := handler(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (sh *strictHandler) SyncEventTypes(w http.ResponseWriter, r *http.Request) {
	handler := func(ctx context.Context) (SyncEventTypesResponseObject, error) {
		return sh.ssi.SyncEventTypes(ctx, SyncEventTypesRequestObject{})
	}
	if _, err := handler(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (sh *strictHandler) GetHealth(w http.ResponseWriter, r *http.Request) {
	handler := func(ctx context.Context) (GetHealthResponseObject, error) {
		return sh.ssi.GetHealth(ctx, GetHealthRequestObject{})
	}
	if _, err := handler(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
