// Package server implements StrictServerInterface — the (ctx,request)->(response,
// error) operations reached only through the generated strict wrapper. Each
// handler exercises a distinct frontier shape so the classifier can bin them:
//
//   - CreateEventTypeTemplate: a NAMED publish + a CLASSIFIED DELETE, both behind
//     the strict-server `$1` seam (the R7/R3 shape).
//   - SyncEventTypes: a DYNAMIC publish (runtime topic) + an OPAQUE write
//     (runtime SQL) — the genuinely-dynamic and the db-call frontiers.
//   - GetHealth: a constant read — the clean read-only control.
package server

import (
	"context"
	"os"

	"example.com/strictsvc/api"
	"example.com/strictsvc/bus"
	"example.com/strictsvc/store"
)

// Server is the StrictServerInterface implementation.
type Server struct {
	st *store.Store
}

// New returns a Server backed by st.
func New(st *store.Store) *Server { return &Server{st: st} }

func (s *Server) CreateEventTypeTemplate(ctx context.Context, _ api.CreateEventTypeTemplateRequestObject) (api.CreateEventTypeTemplateResponseObject, error) {
	bus.Publish("eventtype.created") // named publish — resolved
	return api.CreateEventTypeTemplateResponseObject{}, s.st.DeleteOutbox(ctx)
}

func (s *Server) SyncEventTypes(ctx context.Context, _ api.SyncEventTypesRequestObject) (api.SyncEventTypesResponseObject, error) {
	bus.Publish(os.Getenv("SYNC_TOPIC"))                                                  // runtime topic → <dynamic>
	return api.SyncEventTypesResponseObject{}, s.st.ExecRaw(ctx, os.Getenv("SYNC_TABLE")) // runtime SQL → opaque
}

func (s *Server) GetHealth(ctx context.Context, _ api.GetHealthRequestObject) (api.GetHealthResponseObject, error) {
	return api.GetHealthResponseObject{}, s.st.Ping(ctx) // constant read
}
