// Package roots discovers the entry points a call graph must be rooted at. For a
// service, main is not enough: the real entry points are the HTTP handlers and
// bus consumers that frameworks register through dynamic dispatch
// (router.HandleFunc, bus.Subscribe), leaving them reachable-but-disconnected in
// a naive graph. roots finds those registration calls and resolves the
// func-value argument of each to a concrete function — a synthetic root tagged
// with its route or topic.
//
//	roots = mains ∪ HTTP handlers ∪ bus consumers ∪ (library) exports
//
// A registration whose handler cannot be resolved to a concrete function is not
// dropped: it is recorded as a blind spot, mirroring the rest of the static
// pipeline's disclose-where-blind discipline.
package roots

import (
	"go/constant"
	"go/token"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
)

// Kind classifies a discovered root.
type Kind string

const (
	KindMain     Kind = "main"     // a command's main function
	KindInit     Kind = "init"     // a package initializer (always runs before main)
	KindHTTP     Kind = "http"     // an HTTP handler registered on a router
	KindConsumer Kind = "consumer" // a bus consumer registered via subscribe
	KindExport   Kind = "export"   // an exported function (library fallback)
	// KindCallback and KindWorker are DECLARED roots (config entrypoints), asserted
	// by FQN because call-resolution provably cannot reach them: a callback's
	// handler value threads through a value-flow chain root discovery runs before
	// the call graph and cannot follow; a worker is reachable but is not an entry
	// point. They are kept distinct from KindConsumer so a reader can tell a
	// discovered route from an author-vouched declaration (provenance), and so a
	// worker — which carries no event name — never pollutes the named-event surface.
	KindCallback Kind = "callback" // a library-dispatched callback, declared by FQN
	KindWorker   Kind = "worker"   // a background goroutine worker, declared by FQN
)

// Root is a synthetic entry point for call-graph construction.
type Root struct {
	Func *ssa.Function
	Kind Kind
	// Name is the route ("POST /loan-application") or topic ("payment.settled")
	// the handler was registered under, or "" for mains and exports.
	Name string
}

// FQN is the root's fully-qualified identity.
func (r Root) FQN() string { return r.Func.RelString(nil) }

// Registrar names a framework registration call whose func-value argument is a
// synthetic root. Arg indices are LOGICAL parameter positions (excluding any
// receiver); root discovery adds the receiver offset when the registrar is a
// method (and adds none for an interface-method invoke, whose receiver is not in
// the arg list), so the same hint shape works for free functions, methods, and
// interface methods.
type Registrar struct {
	PkgPath string // import path of the registrar's package, e.g. "net/http"
	Name    string // function/method name, e.g. "HandleFunc" or "Get"
	Kind    Kind   // the kind of root its handler argument becomes
	// Method, when non-empty, is the HTTP method the registration function
	// implies by its name — for routers that expose a function per method
	// (chi's Get/Post/…) rather than encoding the method in the route string.
	// The route is then NameArg's path only, and the root's Name is
	// "<Method> <route>", matching the "POST /loan-application" form ServeMux
	// produces directly.
	Method     string
	NameArg    int // logical position of the route/topic string, or -1 if none
	HandlerArg int // logical position of the func-value argument
}

// DeclaredEntrypoint is an author-asserted root that call-resolution cannot reach:
// a library-dispatched callback (KindCallback) or a background goroutine worker
// (KindWorker). PkgPath and Symbol identify the function — an "import/path#Symbol"
// reference the caller has already split; Ref is that reference verbatim, used as
// the root's disclosed Name so a reader sees the exact config entry the root was
// asserted from.
//
// Resolution is by (PkgPath, Symbol) over the program's first-party functions.
// A symbol naming several methods (same name, different receivers) roots ALL
// matches: over-approximate and SOUND, because a declared root only ever turns
// provenAbsent → reachable, never the reverse, so an extra match cannot manufacture
// a false proof of absence (CLAUDE.md tenet 4). The match set is order-independent
// (it is a set, sorted before output), so rooting stays deterministic. A symbol
// that matches nothing is disclosed as a blind spot — a stale declaration is drift,
// surfaced rather than silently dropped.
type DeclaredEntrypoint struct {
	PkgPath string
	Symbol  string
	Kind    Kind
	Ref     string
}

// BlindSpot records a registration whose handler could not be resolved to a
// concrete function — an unresolved dynamic dispatch at an entry point.
type BlindSpot struct {
	Registrar string // the registrar that was called
	Pos       string // source position of the call
	Detail    string // why the handler could not be resolved
}

// Result is the discovered root set plus disclosed gaps, both sorted for
// determinism.
type Result struct {
	Roots      []Root
	BlindSpots []BlindSpot
}

// DiscoveredRootCount is the number of distinct DISCOVERED entry-point functions —
// mains, HTTP handlers, bus consumers, and library exports. It EXCLUDES package
// initializers: init runs universally before main and is seeded into RTA only to
// recover addresses taken during initialization (the registration idiom), so it is
// an implicit analysis seed, not a discovered service entry point. The call-graph
// provenance caveat counts these, so the number is the service's real entry surface
// and stays stable whether or not a package happens to carry an init.
func (r *Result) DiscoveredRootCount() int {
	seen := make(map[*ssa.Function]bool, len(r.Roots))
	for _, rt := range r.Roots {
		if rt.Kind == KindInit {
			continue
		}
		seen[rt.Func] = true
	}
	return len(seen)
}

// Funcs returns the distinct root functions, in Result.Roots order — the input
// RTA and the other algorithms expect. Distinct because two routes may share one
// handler function (each is its own Root), while the graph algorithms root at
// functions. It INCLUDES init roots: RTA must seed from init to see registration
// addresses taken there (DiscoveredRootCount is the entry-surface count instead).
func (r *Result) Funcs() []*ssa.Function {
	fns := make([]*ssa.Function, 0, len(r.Roots))
	seen := make(map[*ssa.Function]bool, len(r.Roots))
	for _, rt := range r.Roots {
		if seen[rt.Func] {
			continue
		}
		seen[rt.Func] = true
		fns = append(fns, rt.Func)
	}
	return fns
}

// HTTPRegistrars are the built-in HTTP registration hints: stdlib's ServeMux
// (the method is in the route string, e.g. "POST /x"), and go-chi's per-method
// router functions (the method is the function name, the route is the path) —
// the latter is how oapi-codegen's chi server registers handlers. Bus registrars
// are service-specific and come from the classification hints, so callers append
// them.
func HTTPRegistrars() []Registrar {
	regs := []Registrar{
		{PkgPath: "net/http", Name: "HandleFunc", Kind: KindHTTP, NameArg: 0, HandlerArg: 1},
	}
	return append(regs, chiRegistrars()...)
}

// chiRegistrars are the go-chi/v5 per-method router functions. chi.Router is an
// interface, so these are matched as interface-method invokes; the HTTP method
// comes from the function name and the route is the (possibly base-URL-prefixed)
// path argument.
func chiRegistrars() []Registrar {
	const chi = "github.com/go-chi/chi/v5"
	methods := []struct{ fn, method string }{
		{"Get", "GET"}, {"Post", "POST"}, {"Put", "PUT"}, {"Delete", "DELETE"},
		{"Patch", "PATCH"}, {"Head", "HEAD"}, {"Options", "OPTIONS"},
		{"Connect", "CONNECT"}, {"Trace", "TRACE"},
	}
	regs := make([]Registrar, 0, len(methods))
	for _, m := range methods {
		regs = append(regs, Registrar{
			PkgPath: chi, Name: m.fn, Kind: KindHTTP, Method: m.method, NameArg: 0, HandlerArg: 1,
		})
	}
	return regs
}

// Discover finds the synthetic roots of prog given the registration hints and any
// author-declared entrypoints (callbacks and workers call-resolution cannot
// reach). When the unit has no mains and no registered or declared handlers, it
// falls back to the unit's exported functions (library mode).
func Discover(prog *ssabuild.Program, registrars []Registrar, declared ...DeclaredEntrypoint) *Result {
	res := &Result{}
	// Identity is the full (fn, kind, name) triple, not the function alone: two
	// routes may register one handler function, and each must survive as its own
	// root — dropping one would silently erase an entrypoint from the gated
	// contract, with the survivor picked by map iteration order.
	type rootKey struct {
		fn   *ssa.Function
		kind Kind
		name string
	}
	seen := make(map[rootKey]bool)

	add := func(fn *ssa.Function, kind Kind, name string) {
		if fn == nil || seen[rootKey{fn, kind, name}] {
			return
		}
		seen[rootKey{fn, kind, name}] = true
		res.Roots = append(res.Roots, Root{Func: fn, Kind: kind, Name: name})
	}

	// mains.
	for _, p := range ssautil.MainPackages(prog.ServicePkgs) {
		if mainFn := p.Func("main"); mainFn != nil {
			add(mainFn, KindMain, "")
		}
	}

	// Package initializers. Every first-party package's synthesized "init" runs
	// unconditionally before main — it executes package-level var initializers and
	// the explicit init() funcs — so it is a genuine, always-taken entry point, not
	// an over-approximation. Rooting it is what lets the graph see addresses taken
	// only in init (the idiomatic registration site: `func init(){ register(h) }` or
	// `var reg = map[string]F{...: h}`); without it, a func value registered only in
	// init resolves to no callee and its handler's effects vanish from the graph —
	// the silent provenAbsent the UnresolvedCall disclosure (blindspots) otherwise
	// has to abstain on. An empty init contributes only an isolated node.
	for _, p := range prog.ServicePkgs {
		if initFn := p.Func("init"); initFn != nil {
			add(initFn, KindInit, "")
		}
	}

	// HTTP handlers and bus consumers, from registration calls in first-party code.
	byKey := indexRegistrars(registrars)
	for fn := range ssautil.AllFunctions(prog.Prog) {
		if fn.Pkg == nil || !prog.IsFirstParty(fn.Pkg) {
			continue
		}
		for _, b := range fn.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				cc := call.Common()
				pkg, fname, recvOffset, ok := calleeKey(cc)
				if !ok {
					continue
				}
				reg, ok := byKey[regKey{pkg, fname}]
				if !ok {
					continue
				}
				handler, name, isReg := resolveRegistration(cc, reg, recvOffset)
				if !isReg {
					continue // matched the name but the handler arg isn't func-typed: not a registration
				}
				if handler == nil {
					res.BlindSpots = append(res.BlindSpots, BlindSpot{
						Registrar: pkg + "." + fname,
						Pos:       posOf(call),
						Detail:    "handler argument is not a statically resolvable function",
					})
					continue
				}
				add(handler, reg.Kind, name)
			}
		}
	}

	// Declared entrypoints: author-asserted roots call-resolution cannot reach
	// (library-dispatched callbacks, background goroutine workers). Resolve each by
	// (package, symbol) over first-party functions and root it directly, bypassing
	// the registration-call resolver that hits the value-flow wall on the
	// manager-holds-handler idiom (the handler threads through a struct field /
	// constructor / closure root discovery cannot follow before the call graph
	// exists). Rooting is monotonic on reachability — provenAbsent → reachable only —
	// so an over-approximate match set (a symbol naming several methods) stays sound;
	// a declaration matching nothing is disclosed as a blind spot, not dropped. This
	// runs before the library-fallback gate so a service whose only entry is a
	// declared callback/worker does not collapse into exported-surface mode.
	for _, d := range declared {
		matched := false
		for fn := range ssautil.AllFunctions(prog.Prog) {
			if fn.Pkg == nil || !prog.IsFirstParty(fn.Pkg) {
				continue
			}
			if pkgPath(fn) == d.PkgPath && fn.Name() == d.Symbol {
				add(fn, d.Kind, d.Ref)
				matched = true
			}
		}
		if !matched {
			res.BlindSpots = append(res.BlindSpots, BlindSpot{
				Registrar: d.Ref,
				Detail:    "declared entrypoint resolves to no first-party function (stale reference or moved code)",
			})
		}
	}

	// Library fallback: no PRIMARY entry point (main, HTTP handler, bus consumer, or
	// declared callback/worker) to root from, so root at every exported function.
	// Init roots are
	// excluded from this test — every package has a synthesized init, so counting
	// them would permanently suppress the fallback and leave a pure library's
	// exported surface unrooted.
	if !hasPrimaryRoot(res.Roots) {
		for _, p := range prog.ServicePkgs {
			for _, m := range p.Members {
				if fn, ok := m.(*ssa.Function); ok && fn.Object() != nil && fn.Object().Exported() {
					add(fn, KindExport, "")
				}
			}
		}
	}

	sortResult(res)
	return res
}

// hasPrimaryRoot reports whether the set contains a primary entry point — a main,
// an HTTP handler, a bus consumer, or a declared callback/worker. Init and export
// roots do not count: init is synthesized for every package (so it is always
// present), and exports ARE the library fallback this predicate gates. A declared
// callback/worker DOES count: it is an author-asserted real entry, so a service
// whose only entry is one must not collapse into exported-surface library mode.
func hasPrimaryRoot(rs []Root) bool {
	for _, r := range rs {
		switch r.Kind {
		case KindMain, KindHTTP, KindConsumer, KindCallback, KindWorker:
			return true
		}
	}
	return false
}

type regKey struct{ pkgPath, name string }

func indexRegistrars(rs []Registrar) map[regKey]Registrar {
	m := make(map[regKey]Registrar, len(rs))
	for _, r := range rs {
		m[regKey{r.PkgPath, r.Name}] = r
	}
	return m
}

// calleeKey identifies the called function for registration matching and the
// offset to add to logical arg positions. A static method call carries its
// receiver at Args[0] (offset 1); a free function and an interface-method invoke
// do not (offset 0) — for an invoke the receiver is cc.Value, outside Args.
func calleeKey(cc *ssa.CallCommon) (pkg, name string, recvOffset int, ok bool) {
	if callee := cc.StaticCallee(); callee != nil {
		// Normalize a generic instantiation to its origin before deriving the key:
		// an instantiated `Register[T]` reports Name() == "Register[pkg.Type]" and an
		// empty package, so it never matches a registrar keyed on (pkgPath, "Register").
		// Origin() yields the declared generic (pkg, name); it is nil for a
		// non-generic function, so this is a no-op off the generic path.
		if orig := callee.Origin(); orig != nil {
			callee = orig
		}
		off := 0
		if callee.Signature.Recv() != nil {
			off = 1
		}
		return pkgPath(callee), callee.Name(), off, true
	}
	if cc.IsInvoke() && cc.Method != nil && cc.Method.Pkg() != nil {
		return cc.Method.Pkg().Path(), cc.Method.Name(), 0, true
	}
	return "", "", 0, false
}

// resolveRegistration extracts the handler function and the registered name from
// one registration call. recvOffset shifts logical positions to skip a receiver
// in the arg list. When the registrar implies an HTTP method by its name (chi's
// Get/Post/…), the name is "<Method> <route>"; otherwise it is the route string
// verbatim (ServeMux's "POST /x"). The route is recovered through string
// concatenation, so an oapi-codegen `baseURL + "/path"` yields "/path".
//
// isRegistration is false when the matched call's handler argument is not
// func-typed: that is an incidental name collision (e.g. a config-declared
// registrar matching an unrelated method, or gin's variadic handler arriving as a
// slice), not a route registration, so the caller skips it silently rather than
// misreporting it as a blind spot. A func-typed but unresolvable handler is a
// genuine blind spot (isRegistration true, fn nil).
func resolveRegistration(cc *ssa.CallCommon, reg Registrar, recvOffset int) (fn *ssa.Function, name string, isRegistration bool) {
	handlerIdx := reg.HandlerArg + recvOffset
	if handlerIdx < 0 || handlerIdx >= len(cc.Args) {
		return nil, "", false
	}
	if !isHandlerArg(cc.Args[handlerIdx]) {
		return nil, "", false
	}
	handler := resolveHandler(cc.Args[handlerIdx])
	route := ""
	if reg.NameArg >= 0 {
		nameIdx := reg.NameArg + recvOffset
		if nameIdx >= 0 && nameIdx < len(cc.Args) {
			route = constStringSegments(cc.Args[nameIdx])
		}
	}
	name = route
	if reg.Method != "" {
		name = reg.Method
		if route != "" {
			name += " " + route
		}
	}
	return handler, name, true
}

// isHandlerArg reports whether v is plausibly a route handler argument: a func
// value, or a slice of funcs (a variadic handler list). This distinguishes a real
// registration from an incidental name collision (e.g. a config registrar
// matching an unrelated method whose argument is not a function).
func isHandlerArg(v ssa.Value) bool {
	switch t := v.Type().Underlying().(type) {
	case *types.Signature:
		return true
	case *types.Slice:
		_, ok := t.Elem().Underlying().(*types.Signature)
		return ok
	}
	return false
}

// constStringSegments recovers the constant parts of a string built by
// concatenation, eliding non-constant operands. A single string constant is
// returned verbatim; `baseURL + "/loan-application/{id}"` (a BinOp with a
// non-constant left operand) yields "/loan-application/{id}". A fully dynamic
// route yields "" (and is disclosed as a blind spot upstream, like any
// unresolvable registration).
func constStringSegments(v ssa.Value) string {
	switch t := v.(type) {
	case *ssa.Const:
		return constString(t)
	case *ssa.BinOp:
		if t.Op == token.ADD {
			return constStringSegments(t.X) + constStringSegments(t.Y)
		}
	}
	return ""
}

// resolveHandler peels framework wrappers off a func-value argument and returns
// the concrete function it denotes, or nil if it is not statically resolvable. It
// unwraps named-type conversions (a method value converted to the registrar's
// handler type) and resolves bound-method and thunk wrappers to the underlying
// method, while leaving genuine closures intact.
func resolveHandler(v ssa.Value) *ssa.Function {
	switch t := v.(type) {
	case *ssa.MakeClosure:
		if fn, ok := t.Fn.(*ssa.Function); ok {
			return realFunc(fn)
		}
	case *ssa.Function:
		return realFunc(t)
	case *ssa.ChangeType:
		return resolveHandler(t.X)
	case *ssa.Convert:
		return resolveHandler(t.X)
	case *ssa.MakeInterface:
		return resolveHandler(t.X)
	case *ssa.Slice:
		// A variadic handler argument (gin's r.GET(path, ...HandlerFunc)) arrives
		// as a slice the caller builds; the real handler is the last element
		// (any leading elements are middleware).
		return variadicLastFunc(t)
	}
	return nil
}

// variadicLastFunc recovers the highest-indexed function stored into the array
// backing a variadic-argument slice — the final handler in r.GET(path, mw…, h).
// Leading middleware elements are intentionally not rooted: rooting one route at
// several handlers would duplicate the entry point, and a middleware that itself
// made a boundary call is the rarer case.
func variadicLastFunc(slice *ssa.Slice) *ssa.Function {
	alloc, ok := slice.X.(*ssa.Alloc)
	if !ok || alloc.Referrers() == nil {
		return nil
	}
	var bestVal ssa.Value
	bestIdx := int64(-1)
	for _, ref := range *alloc.Referrers() {
		idx, ok := ref.(*ssa.IndexAddr)
		if !ok || idx.X != alloc || idx.Referrers() == nil {
			continue
		}
		ci, ok := idx.Index.(*ssa.Const)
		if !ok || ci.Value == nil || ci.Value.Kind() != constant.Int {
			continue
		}
		i, exact := constant.Int64Val(ci.Value)
		if !exact || i <= bestIdx {
			continue
		}
		for _, r2 := range *idx.Referrers() {
			if st, ok := r2.(*ssa.Store); ok && st.Addr == idx {
				bestVal, bestIdx = st.Val, i
			}
		}
	}
	if bestVal == nil {
		return nil
	}
	return resolveHandler(bestVal)
}

// realFunc resolves a synthetic bound-method or thunk wrapper to the method it
// wraps, so the root carries the real handler's identity rather than a
// "$bound"/"$thunk" name. Non-synthetic functions (declared funcs, closures) are
// returned unchanged.
func realFunc(fn *ssa.Function) *ssa.Function {
	if fn.Synthetic == "" {
		return fn
	}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			if call, ok := instr.(ssa.CallInstruction); ok {
				if callee := call.Common().StaticCallee(); callee != nil {
					return callee
				}
			}
		}
	}
	return fn
}

func resultLess(a, b Root) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.FQN() < b.FQN()
}

func sortResult(res *Result) {
	sort.Slice(res.Roots, func(i, j int) bool { return resultLess(res.Roots[i], res.Roots[j]) })
	sort.Slice(res.BlindSpots, func(i, j int) bool {
		a, b := res.BlindSpots[i], res.BlindSpots[j]
		if a.Registrar != b.Registrar {
			return a.Registrar < b.Registrar
		}
		if a.Pos != b.Pos {
			return a.Pos < b.Pos
		}
		return a.Detail < b.Detail
	})
}

// pkgPath returns the import path of fn's package, or "" for synthetic functions
// with no package.
func pkgPath(fn *ssa.Function) string {
	if fn.Pkg == nil || fn.Pkg.Pkg == nil {
		return ""
	}
	return fn.Pkg.Pkg.Path()
}

// constString returns the Go string value of v if it is a constant string, else
// "" — a non-constant name is not statically knowable.
func constString(v ssa.Value) string {
	c, ok := v.(*ssa.Const)
	if !ok || c.Value == nil || c.Value.Kind() != constant.String {
		return ""
	}
	return constant.StringVal(c.Value)
}

func posOf(instr ssa.Instruction) string {
	if instr.Parent() == nil {
		return ""
	}
	fset := instr.Parent().Prog.Fset
	return fset.Position(instr.Pos()).String()
}
