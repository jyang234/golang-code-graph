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
	KindHTTP     Kind = "http"     // an HTTP handler registered on a router
	KindConsumer Kind = "consumer" // a bus consumer registered via subscribe
	KindExport   Kind = "export"   // an exported function (library fallback)
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

// Funcs returns just the root functions, in Result.Roots order — the input RTA
// and the other algorithms expect.
func (r *Result) Funcs() []*ssa.Function {
	fns := make([]*ssa.Function, len(r.Roots))
	for i, rt := range r.Roots {
		fns[i] = rt.Func
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

// Discover finds the synthetic roots of prog given the registration hints. When
// the unit has no mains and no registered handlers, it falls back to the unit's
// exported functions (library mode).
func Discover(prog *ssabuild.Program, registrars []Registrar) *Result {
	res := &Result{}
	seen := make(map[*ssa.Function]bool)

	add := func(fn *ssa.Function, kind Kind, name string) {
		if fn == nil || seen[fn] {
			return
		}
		seen[fn] = true
		res.Roots = append(res.Roots, Root{Func: fn, Kind: kind, Name: name})
	}

	// mains.
	for _, p := range ssautil.MainPackages(prog.ServicePkgs) {
		if mainFn := p.Func("main"); mainFn != nil {
			add(mainFn, KindMain, "")
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

	// Library fallback: nothing to root from, so root at every exported function.
	if len(res.Roots) == 0 {
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
