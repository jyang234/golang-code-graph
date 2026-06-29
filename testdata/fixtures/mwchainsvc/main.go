// Command mwchainsvc is the fixture for the middleware-chain reclaimer: the
// oapi-codegen / chi middleware-application loop
//
//	for _, mw := range siw.HandlerMiddlewares { h = mw(h) }; h.ServeHTTP(w, r)
//
// applied at every route. The `mw(h)` call is a func-value call the builder resolves to no
// callee (an UnresolvedCall blind spot), so an entrypoint-anchored absence proof whose cone
// crosses the loop abstains. The fixture exercises the soundness poles the reclaimer must
// separate, each on its OWN middleware element type and OWN wrapper type so the call-graph
// algorithm cannot cross-contaminate them: VTA resolves a func-value call to the values that
// flow to a slice element of that exact type, so a SHARED middleware type would leak the
// concrete funcs of one scenario into the empty loop of another and hide the very seam under
// test.
//
//   - EmptyMW / EmptyWrapper — HandlerMiddlewares never populated (the reproducer / the
//     nil-in-prod oapi-codegen shape), and EmptyMW has NO concrete value anywhere, so the
//     loop is genuinely zero-resolution. The reclaimer recovers caller→business and CLEARS
//     the seam, making the read route provably read-only and the write route a determinate
//     violation.
//   - KnownMW / KnownWrapper — HandlerMiddlewares wired from a const slice literal
//     {auth, logging}. The reclaimer resolves `mw(h)` to those concrete funcs and recovers
//     the business terminal, but LEAVES the seam disclosed (each middleware re-dispatches
//     through its own next.ServeHTTP, a hop this pass does not chase).
//   - AppendMW / AppendWrapper — HandlerMiddlewares built by an append chain of known funcs
//     (the hand-written-stack shape). The reclaimer traces the append chain to {authA, logA}.
//   - DynMW / DynWrapper — HandlerMiddlewares populated from an unknown source (an opaque
//     package var). The set is not provable, so the reclaimer recovers NO middleware edge and
//     the seam STAYS blind — the soundness pole.
//   - EscapeMW / EscapeWrapper — HandlerMiddlewares whose slice escapes into a helper that
//     could mutate its backing array past the store walk, so the set is not provable even
//     though no element is stored: the reclaimer abstains (the completeness guard).
package main

import "net/http"

// ---- shared business handlers and effects ----

func businessGetItems(w http.ResponseWriter, r *http.Request)  { readOnlyWork() }
func businessPostItems(w http.ResponseWriter, r *http.Request) { dbWrite() }
func readOnlyWork()                                            {}
func dbWrite()                                                 {}

// ---- EmptyMW / EmptyWrapper: the reproducer (HandlerMiddlewares never populated) ----

type EmptyMW func(http.Handler) http.Handler

type EmptyWrapper struct {
	HandlerMiddlewares []EmptyMW
}

func (siw *EmptyWrapper) apply(h http.Handler) http.Handler {
	for _, mw := range siw.HandlerMiddlewares {
		h = mw(h)
	}
	return h
}

func (siw *EmptyWrapper) GetItems(w http.ResponseWriter, r *http.Request) {
	siw.apply(http.HandlerFunc(businessGetItems)).ServeHTTP(w, r)
}

func (siw *EmptyWrapper) PostItems(w http.ResponseWriter, r *http.Request) {
	siw.apply(http.HandlerFunc(businessPostItems)).ServeHTTP(w, r)
}

// ---- KnownMW / KnownWrapper: HandlerMiddlewares wired from a const slice literal ----

type KnownMW func(http.Handler) http.Handler

func auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authWork()
		next.ServeHTTP(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logWork()
		next.ServeHTTP(w, r)
	})
}

func authWork() {}
func logWork()  {}

type KnownWrapper struct {
	HandlerMiddlewares []KnownMW
}

func (siw *KnownWrapper) apply(h http.Handler) http.Handler {
	for _, mw := range siw.HandlerMiddlewares {
		h = mw(h)
	}
	return h
}

func (siw *KnownWrapper) Route(w http.ResponseWriter, r *http.Request) {
	siw.apply(http.HandlerFunc(businessGetItems)).ServeHTTP(w, r)
}

func newKnown() *KnownWrapper {
	return &KnownWrapper{HandlerMiddlewares: []KnownMW{auth, logging}}
}

// ---- AppendMW / AppendWrapper: HandlerMiddlewares built by an append chain of known funcs ----

type AppendMW func(http.Handler) http.Handler

func authA(next http.Handler) http.Handler { return next }
func logA(next http.Handler) http.Handler  { return next }

type AppendWrapper struct {
	HandlerMiddlewares []AppendMW
}

func (siw *AppendWrapper) apply(h http.Handler) http.Handler {
	for _, mw := range siw.HandlerMiddlewares {
		h = mw(h)
	}
	return h
}

func (siw *AppendWrapper) Route(w http.ResponseWriter, r *http.Request) {
	siw.apply(http.HandlerFunc(businessGetItems)).ServeHTTP(w, r)
}

func newAppend() *AppendWrapper {
	w := &AppendWrapper{}
	w.HandlerMiddlewares = append(w.HandlerMiddlewares, authA)
	w.HandlerMiddlewares = append(w.HandlerMiddlewares, logA)
	return w
}

// ---- DynMW / DynWrapper: HandlerMiddlewares from an unknown source ----

type DynMW func(http.Handler) http.Handler

// dynamicMiddlewares is a package var the reclaimer cannot enumerate: a global's contents
// can be set from package init, another package, reflection, or linkname, so a sound tracer
// must refuse to assume it empty. The field store reads this opaque value, so the reclaimer
// abstains (no edge, no clear) and the seam stays UnresolvedCall — the soundness pole.
var dynamicMiddlewares []DynMW

type DynWrapper struct {
	HandlerMiddlewares []DynMW
}

func (siw *DynWrapper) apply(h http.Handler) http.Handler {
	for _, mw := range siw.HandlerMiddlewares {
		h = mw(h)
	}
	return h
}

func (siw *DynWrapper) Route(w http.ResponseWriter, r *http.Request) {
	siw.apply(http.HandlerFunc(businessPostItems)).ServeHTTP(w, r)
}

func newDynamic() *DynWrapper {
	return &DynWrapper{HandlerMiddlewares: dynamicMiddlewares}
}

// ---- EscapeMW / EscapeWrapper: the field slice escapes into a helper ----

type EscapeMW func(http.Handler) http.Handler

type EscapeWrapper struct {
	HandlerMiddlewares []EscapeMW
}

// register hands the field slice to a function that could mutate its backing array past the
// store walk, so even though no element is stored the set is NOT provable — the reclaimer
// must abstain (no edge, no clear) rather than assume empty.
func register(mws []EscapeMW) { escapeSink = mws }

var escapeSink []EscapeMW

func (siw *EscapeWrapper) apply(h http.Handler) http.Handler {
	register(siw.HandlerMiddlewares)
	for _, mw := range siw.HandlerMiddlewares {
		h = mw(h)
	}
	return h
}

func (siw *EscapeWrapper) Route(w http.ResponseWriter, r *http.Request) {
	siw.apply(http.HandlerFunc(businessPostItems)).ServeHTTP(w, r)
}

func main() {
	empty := &EmptyWrapper{}
	known := newKnown()
	app := newAppend()
	dyn := newDynamic()
	esc := &EscapeWrapper{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", empty.GetItems)
	mux.HandleFunc("POST /items", empty.PostItems)
	mux.HandleFunc("GET /known", known.Route)
	mux.HandleFunc("GET /append", app.Route)
	mux.HandleFunc("GET /dyn", dyn.Route)
	mux.HandleFunc("GET /esc", esc.Route)

	srv := &http.Server{Addr: ":0", Handler: mux}
	_ = srv.ListenAndServe()
}
