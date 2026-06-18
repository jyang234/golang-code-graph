// Command reclaimsvc is the adversarial fixture for probe #3: the StrictServer
// reclaimer's R2 soundness boundary. It replicates the oapi-codegen strict-server
// dispatch shape by hand (a per-handler http.HandlerFunc closure, wrapped through a
// middleware loop, dispatched via the http.Handler interface) and then stresses the
// flowsTo flow-requirement that keeps the reclaimer from adding an edge real
// execution cannot take:
//
//   - Admin builds the SERVED closure (Admin$1, which performs the forbidden DB
//     DELETE and reaches the ServeHTTP receiver through the middleware Phi) AND a
//     SIBLING closure it merely passes to runLater (never served). The reclaimer
//     must connect Admin only to the served closure, not the sibling.
//   - authMW supplies the middleware Phi the served closure flows through, and can
//     SHORT-CIRCUIT (no Authorization header → 401, the handler is never called). The
//     reclaimed Admin→Admin$1 edge is therefore a MAY edge: execution CAN take it
//     (the authorized path), which is exactly R2's "can take" requirement — a
//     spurious edge would only ever over-block a negative check, never fabricate a
//     false provenAbsent. (authMW's own ServeHTTP call lives inside its returned
//     closure, not its body, so authMW is not itself a reclaim candidate.)
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
)

type store struct{ db *sql.DB }

// Delete is the forbidden DB write, reachable ONLY through the served handler
// closure — never called directly.
func (s *store) Delete(ctx context.Context) {
	const q = "DELETE FROM ledger WHERE id = $1"
	_, _ = s.db.ExecContext(ctx, q, "L1")
}

type middleware func(http.Handler) http.Handler

// authMW conditionally short-circuits: with no Authorization it returns 401 and
// never calls next; otherwise it passes through. It is the middleware in Admin's wrap
// loop, so it produces the Phi the served closure flows through; its short-circuit is
// what makes the reclaimed edge a may-edge rather than a must-edge.
func authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return // short-circuit: next is NOT invoked on this path
		}
		next.ServeHTTP(w, r) // pass-through: the served closure IS invoked
	})
}

// wrapper replicates the strict-server ServerInterfaceWrapper.
type wrapper struct {
	s   *store
	mws []middleware
}

// sink stores callbacks but never invokes them — a place a closure can be PASSED
// without ever being served.
var sink []func()

func runLater(f func()) { sink = append(sink, f) }

// Admin is the strict-server-shaped dispatch method under test.
func (wr *wrapper) Admin(w http.ResponseWriter, r *http.Request) {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wr.s.Delete(r.Context()) // the forbidden effect, behind the dispatch seam
	})
	for _, mw := range wr.mws {
		handler = mw(handler)
	}
	// A sibling closure the method only PASSES — never served. Must not be reclaimed.
	runLater(func() { _ = "audit note, never served" })
	handler.ServeHTTP(w, r)
}

func main() {
	wr := &wrapper{s: &store{}, mws: []middleware{authMW}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin", wr.Admin)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
