// Package reclaim holds the sound static reclaimers from
// docs/design/frontier-instrumentation-plan.md, Phase 3: passes that ADD the
// call edges the builder lost at a recognized framework dispatch seam, shrinking
// the Category-B frontier. Every reclaimer obeys rule R2 — it emits only edges
// real execution can take, so adding them can never manufacture a false proof of
// absence (an added true edge turns provenAbsent→reachable, never the reverse).
// Reclaimers are opt-in (D2) and their edges carry provenance (the Via field).
package reclaim

import (
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
)

// ViaStrictServer attributes an edge to the strict-server reclaimer.
const ViaStrictServer = "strict-server"

// ViaTxClosure attributes an edge to the tx-closure reclaimer.
const ViaTxClosure = "tx-closure"

// Edge is a sound, reclaimer-attributed call edge to add to the graph: From and To
// are node FQNs (RelString form), Via names the reclaimer that recovered it.
type Edge struct {
	From string
	To   string
	Via  string
}

// StrictServer reclaims the oapi-codegen strict-server dispatch seam. The generated
// ServerInterfaceWrapper method builds an http.HandlerFunc from a per-handler
// closure, optionally wraps it through middleware, and dispatches via the
// http.Handler interface (handler.ServeHTTP) — a hop the call-graph builder does
// not cross, so the wrapper method's static reach is starved before its own `$1`
// closure (frontier B / the R3 forward-starvation). For each such method it returns
// the recovered edge wrapper→closure.
//
// Soundness (R2): the edge is emitted ONLY when the closure value provably FLOWS,
// within the method, to the receiver of a ServeHTTP call — traced back through
// MakeClosure / MakeInterface / ChangeType / Convert / Phi to the closure itself.
// When HandlerMiddlewares is empty the wrap loop does not run and
// handler.ServeHTTP(w,r) IS the closure invocation directly, so the closure CAN be
// invoked from the method: a true can-reach edge. The flow requirement is what
// keeps a closure the method merely PASSES elsewhere (a sort comparator, a route
// registration) from being falsely connected — those never reach a ServeHTTP
// receiver, so they are not reclaimed.
func StrictServer(res *analyze.Result) []Edge {
	var edges []Edge
	seen := map[[2]string]bool{}
	for _, n := range res.Graph.Nodes {
		f := n.Func
		if f == nil || len(f.AnonFuncs) == 0 {
			continue
		}
		recvs := serveHTTPReceivers(f)
		if len(recvs) == 0 {
			continue
		}
		for _, c := range f.AnonFuncs {
			if !anyFlowsTo(recvs, c) {
				continue
			}
			key := [2]string{n.FQN, c.RelString(nil)}
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, Edge{From: n.FQN, To: c.RelString(nil), Via: ViaStrictServer})
		}
	}
	return edges
}

// serveHTTPReceivers returns the receiver value of every net/http ServeHTTP invoke
// (`handler.ServeHTTP(w, r)` on an http.Handler interface) in f.
//
// The method is matched by net/http PACKAGE + name, not the bare name "ServeHTTP":
// the reclaimer's soundness rests on `http.HandlerFunc.ServeHTTP(w,r)` calling the
// underlying func, which is a property of net/http specifically. An unrelated
// interface that merely declares a method named ServeHTTP need not invoke the
// closure flowing to it, so matching it could add an edge real execution does not
// take (an R2 violation) and would mis-attribute it to the strict-server seam.
func serveHTTPReceivers(f *ssa.Function) []ssa.Value {
	var out []ssa.Value
	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			c := call.Common()
			if c.IsInvoke() && c.Method != nil && c.Method.Name() == "ServeHTTP" &&
				c.Method.Pkg() != nil && c.Method.Pkg().Path() == "net/http" {
				out = append(out, c.Value)
			}
		}
	}
	return out
}

// TxClosure reclaims the higher-order-call seam where a function hands a closure to a
// shared runner that INVOKES it — the tx-runner shape `u.RunInTx(ctx, func(exec){…writes…})`
// and its generic wrapper `RunInTxResult[T](ctx, u, fn)` (which wraps fn BY REFERENCE
// into a closure it hands to RunInTx). Because the call graph is context-insensitive,
// the call into the runner — especially a generic instantiation — is frequently not
// resolved to a traversable edge, so the closure subtree (and the writes inside it) is
// orphaned from the enclosing function: a write-surface gate (io_budget, effect_ratchet)
// then reads a writing command as 0 effects — a false-clean, the same incomplete-collection
// class as a false NO-FLOW. For each closure passed as a func-value argument to a static
// callee that PROVABLY invokes the corresponding parameter, it returns the recovered edge
// enclosing-fn→closure.
//
// Soundness (R2): the edge is emitted ONLY when the callee provably invokes the parameter
// the closure is bound to — directly (the parameter is the callee value of a call), by
// FORWARDING it to another static callee that invokes the corresponding parameter, or by
// WRAPPING it by reference into a closure that loads-and-calls it and is itself invoked
// (the RunInTxResult shape: param→*ssa.Alloc→free var→load+call). So the closure CAN be
// invoked when the enclosing function runs — a real can-reach edge, never a manufactured
// one (adding a true edge only ever turns provenAbsent→reachable). A closure handed to a
// callee that merely STORES the parameter (a registry that never invokes it) is NOT
// connected — invokesParam returns false — the R2 negative. Provenance-tagged via=tx-closure.
func TxClosure(res *analyze.Result) []Edge {
	var edges []Edge
	seen := map[[2]string]bool{}
	// One memoising impl-finder shared across the whole pass: the interface resolution is
	// call-site-independent, so this scans prog.RuntimeTypes() once per (interface, method).
	finder := NewImplFinder(progOf(res))
	for _, n := range res.Graph.Nodes {
		f := n.Func
		if f == nil {
			continue
		}
		for _, b := range f.Blocks {
			for _, instr := range b.Instrs {
				call, ok := instr.(ssa.CallInstruction)
				if !ok {
					continue
				}
				common := call.Common()
				callee := common.StaticCallee()
				if callee == nil {
					continue
				}
				for i, arg := range common.Args {
					closure := ClosureFn(arg)
					if closure == nil {
						continue
					}
					if !newTxProbe(finder).invokesParam(callee, i) {
						continue
					}
					key := [2]string{n.FQN, closure.RelString(nil)}
					if seen[key] {
						continue
					}
					seen[key] = true
					edges = append(edges, Edge{From: n.FQN, To: closure.RelString(nil), Via: ViaTxClosure})
				}
			}
		}
	}
	return edges
}

// ClosureFn returns the anonymous function behind a closure value passed as a call
// argument — a *ssa.MakeClosure, optionally behind the type conversions a func-value
// argument can carry — or nil if the argument is not a closure. It deliberately matches
// only MakeClosure (the orphaned-closure case), not a bare *ssa.Function: a top-level
// func passed as a value is address-taken and already resolved by the call-graph builder.
// Exported as the one source of truth for "is this call argument a closure", shared by
// the TxClosure reclaimer and the experimental rebind de-union pass.
func ClosureFn(v ssa.Value) *ssa.Function {
	switch x := v.(type) {
	case *ssa.MakeClosure:
		fn, _ := x.Fn.(*ssa.Function)
		return fn
	case *ssa.ChangeType:
		return ClosureFn(x.X)
	case *ssa.Convert:
		return ClosureFn(x.X)
	case *ssa.MakeInterface:
		return ClosureFn(x.X)
	}
	return nil
}

// DirectlyInvokesParam reports whether fn invokes its idx-th parameter DIRECTLY — i.e.
// there is a call whose callee VALUE is that parameter (`fn(args…)` where fn is the
// parameter). This is precisely the shape under which the call graph carries a concrete
// fn→closure union edge AT fn's own call site (the shared-runner `RunInTx`'s fn(exec)),
// so it is the predicate the experimental rebind de-union pass keys on to know which
// union edge it may rebind. It does NOT consider the generic-wrapper / forwarding cases
// invokesParam handles — those fan at a deeper, non-fn site, so their union is not a
// single removable fn→closure edge. One source of truth for the direct-invoker test.
func DirectlyInvokesParam(fn *ssa.Function, idx int) bool {
	if fn == nil || idx < 0 || idx >= len(fn.Params) {
		return false
	}
	p := fn.Params[idx]
	for _, ref := range referrers(p) {
		if call, ok := ref.(ssa.CallInstruction); ok {
			c := call.Common()
			if c.Value == p && !c.IsInvoke() {
				return true
			}
		}
	}
	return false
}

// invKey identifies the query "does fn invoke its idx-th parameter".
type invKey struct {
	fn  *ssa.Function
	idx int
}

// txProbe carries the visited sets that bound the mutual recursion between invokesParam
// ("does fn invoke its idx-th parameter") and valueInvoked ("is this func-value invoked").
// A revisit returns false — conservatively "not proven via this path", which can only
// lose precision (miss a reclaimable closure), never soundness (invent an edge); and
// adding edges is the safe direction regardless. One probe per top-level query keeps the
// result a pure function of the SSA, so TxClosure is deterministic. impls resolves
// interface-dispatched runner calls to their concrete implementations (the dominant real
// shape: the unit-of-work is an interface), memoised across the whole pass.
type txProbe struct {
	impls *ImplFinder
	param map[invKey]bool
	val   map[ssa.Value]bool
}

func newTxProbe(impls *ImplFinder) *txProbe {
	return &txProbe{impls: impls, param: map[invKey]bool{}, val: map[ssa.Value]bool{}}
}

// invokesParam reports whether calling fn provably invokes the function value passed as
// its idx-th parameter.
func (p *txProbe) invokesParam(fn *ssa.Function, idx int) bool {
	if fn == nil || idx < 0 || idx >= len(fn.Params) {
		return false
	}
	k := invKey{fn, idx}
	if p.param[k] {
		return false
	}
	p.param[k] = true
	return p.valueInvoked(fn.Params[idx], fn)
}

// valueInvoked reports whether the function value v is provably invoked within the
// function `within` — called directly, forwarded to a static callee that invokes the
// corresponding parameter, or stored into a local cell captured by reference into a
// closure that loads-and-calls it and is itself invoked.
func (p *txProbe) valueInvoked(v ssa.Value, within *ssa.Function) bool {
	if v == nil || p.val[v] {
		return false
	}
	p.val[v] = true
	for _, ref := range referrers(v) {
		switch r := ref.(type) {
		case ssa.CallInstruction:
			common := r.Common()
			if common.Value == v && !common.IsInvoke() {
				return true // v is the callee value — directly invoked
			}
			if common.IsInvoke() {
				// Interface-dispatched call (the dominant real shape: u.RunInTx where u is
				// an interface). No static callee, so resolve the interface method to its
				// concrete implementations and recurse. common.Args EXCLUDES the receiver,
				// so arg index i maps to an implementation's parameter i+1.
				for i, arg := range common.Args {
					if arg == v && p.allImplsInvokeParam(common, i+1) {
						return true
					}
				}
			} else if callee := common.StaticCallee(); callee != nil {
				for i, arg := range common.Args {
					if arg == v && p.invokesParam(callee, i) {
						return true // forwarded to a callee that invokes that parameter
					}
				}
			}
		case *ssa.Store:
			if r.Val == v {
				if alloc, ok := r.Addr.(*ssa.Alloc); ok && p.allocCapturedAndInvoked(alloc, within) {
					return true
				}
			}
		case *ssa.ChangeType:
			if p.valueInvoked(r, within) {
				return true
			}
		case *ssa.Convert:
			if p.valueInvoked(r, within) {
				return true
			}
		case *ssa.MakeInterface:
			if p.valueInvoked(r, within) {
				return true
			}
		}
	}
	return false
}

// allocCapturedAndInvoked reports whether the local cell alloc is captured BY REFERENCE
// into a closure that loads-and-calls its contents AND that closure is itself invoked
// within `within` — the generic tx-wrapper shape (param→*ssa.Alloc→free var→load+call).
func (p *txProbe) allocCapturedAndInvoked(alloc *ssa.Alloc, within *ssa.Function) bool {
	for _, ref := range referrers(alloc) {
		mc, ok := ref.(*ssa.MakeClosure)
		if !ok {
			continue
		}
		idx := bindingIndex(mc, alloc)
		if idx < 0 {
			continue
		}
		wrapper, ok := mc.Fn.(*ssa.Function)
		if !ok || idx >= len(wrapper.FreeVars) {
			continue
		}
		if p.freeVarLoadInvoked(wrapper, idx) && p.valueInvoked(mc, within) {
			return true
		}
	}
	return false
}

// allImplsInvokeParam reports whether EVERY concrete implementation of the interface
// method called by common provably invokes its paramIdx-th parameter. "Every" (not
// "some") is what keeps the recovered edge unconditionally R2-sound: if every possible
// dynamic implementation invokes the parameter, the closure is invoked no matter which
// type the receiver holds, so the enclosing-fn→closure edge is one real execution can
// always take — independent of how precisely the dispatch is resolved. If the interface
// resolves to no implementation, or any implementation has no analyzable body, or any
// implementation might not invoke the parameter, it abstains (the reclaimer refuses an
// edge it cannot prove).
func (p *txProbe) allImplsInvokeParam(common *ssa.CallCommon, paramIdx int) bool {
	impls := p.implementations(common)
	if len(impls) == 0 {
		return false
	}
	for _, impl := range impls {
		if len(impl.Blocks) == 0 || !p.invokesParam(impl, paramIdx) {
			return false
		}
	}
	return true
}

// implementations resolves the interface method called by common to the concrete
// implementations, via the pass-shared memoising finder (one source of truth, shared with
// the rebind de-union pass through reclaim.Implementations / ImplFinder).
func (p *txProbe) implementations(common *ssa.CallCommon) []*ssa.Function {
	return p.impls.Of(common)
}

// Implementations resolves the interface method called by common to the concrete
// *ssa.Function implementations over prog's runtime type set (CHA). It iterates
// prog.RuntimeTypes() — the actual dynamic types that flow into interfaces, pointer types
// included — so it does not hand-roll a MethodSet(T) walk that would omit pointer-receiver
// (*T) methods (CLAUDE.md "collect functions completely"). Returns nil for a non-invoke
// call. The result order follows prog.RuntimeTypes(); callers that need a canonical order
// sort the FQNs they derive.
func Implementations(prog *ssa.Program, common *ssa.CallCommon) []*ssa.Function {
	meth := common.Method
	if meth == nil || prog == nil {
		return nil
	}
	iface, ok := common.Value.Type().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	return implementationsOver(prog, iface, meth)
}

// implementationsOver is the resolution kernel: the concrete implementations of meth over
// the types in prog.RuntimeTypes() that implement iface. It is a pure function of
// (prog, iface, meth) — which is why ImplFinder can memoise it.
//
// For a VALUE-receiver method, prog.RuntimeTypes() carries BOTH T and *T, and
// prog.MethodValue(*T) is a synthesized promotion wrapper, not the real method (the dominant
// real-fleet shape: event-bus's SQLUnitOfWork.RunInTx is a value receiver). unwrapPromotion
// collapses that wrapper to the value method it delegates to, and the dedup folds the T / *T
// pair into the one real impl — so the "every impl invokes" guard (allImplsInvokeParam) is not
// poisoned by a wrapper whose body invokesParam cannot trace (a false abstain that, on a SHARED
// interface resolution, would silently spread to every command dispatching through it).
//
// Dedup keeps the first occurrence. prog.RuntimeTypes() ranges a map, so its order is NOT
// stable run-to-run; output determinism does not rest on this slice's order but on every
// consumer being order-insensitive — allImplsInvokeParam is an all-quantifier, rebind sorts its
// removals — exactly the contract Implementations documents below.
func implementationsOver(prog *ssa.Program, iface *types.Interface, meth *types.Func) []*ssa.Function {
	var out []*ssa.Function
	seen := map[*ssa.Function]bool{}
	for _, T := range prog.RuntimeTypes() {
		if !types.Implements(T, iface) {
			continue
		}
		sel := prog.MethodSets.MethodSet(T).Lookup(meth.Pkg(), meth.Name())
		if sel == nil {
			continue
		}
		fn := unwrapPromotion(prog.MethodValue(sel))
		if fn == nil || seen[fn] {
			continue
		}
		seen[fn] = true
		out = append(out, fn)
	}
	return out
}

// unwrapPromotion resolves a synthesized *T promotion wrapper to the value method (T).M it
// delegates to. For a value-receiver method M, go/ssa synthesizes prog.MethodValue(*T) as a
// wrapper (`t0 = ssa:wrapnilchk(recv); t1 = *t0; tail-call (T).M(t1, args…)`) whose own body
// does NOT invoke M's func-value parameter — so invokesParam / DirectlyInvokesParam read FALSE
// on it even though the real value method does invoke. The wrapper is not a distinct dynamic
// target — it IS (T).M — so resolving it to its sole same-named static delegate is EXACT: the
// wrapper invokes its parameter iff its delegate does, keeping the reclaimer R2-sound.
//
// The unwrap fires ONLY on a genuine promotion/indirection wrapper, identified by go/ssa's
// Synthetic provenance prefix "wrapper for " (see go/ssa createWrapper). This is deliberately
// NARROWER than "any synthetic function": a generic method INSTANCE is also synthetic
// (Synthetic "instance of M") but has a real body and an SSA name (M[targs]) that go/ssa marks
// "may not be unique" — scanning its body for a same-named static callee could substitute an
// unrelated, name-colliding function as a bogus "delegate", which on rebind's edge-REMOVE path
// would be a false absence and on TxClosure a false edge (an R2 violation). Gating on the
// wrapper prefix excludes instances, thunks ("thunk for "), and bound methods, so a real method
// and every non-wrapper synthetic is returned unchanged (identity) — only an actual promotion
// wrapper is ever collapsed into its delegate.
func unwrapPromotion(fn *ssa.Function) *ssa.Function {
	if fn == nil || !strings.HasPrefix(fn.Synthetic, "wrapper for ") {
		return fn // real method or non-wrapper synthetic: identity
	}
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			if callee := call.Common().StaticCallee(); callee != nil && callee.Name() == fn.Name() {
				return callee // the wrapper's sole delegation to (T).M
			}
		}
	}
	return fn
}

// implKey identifies an interface-method resolution. The interface is keyed by its
// canonical String() (NOT the *types.Interface pointer, which go/types does not intern):
// structurally identical interfaces resolve identically, so sharing is sound; the method
// pointer disambiguates same-shaped interfaces that declare different methods. Keying on
// the method ALONE would be a soundness bug — the SAME *types.Func can be dispatched
// through different interfaces (an embedding), whose impl sets differ, and an undersized
// set could wrongly de-union (a false absence).
type implKey struct {
	iface string
	meth  *types.Func
}

// ImplFinder memoises interface-method → implementations resolution across call sites
// within ONE pass (the resolution is call-site-independent), so a pass with many
// interface-dispatched sites does the RuntimeTypes scan once per (interface, method)
// rather than once per site. A finder is single-pass and not safe for concurrent use; the
// passes that use it (TxClosure, rebind.Compute) are sequential and deterministic.
type ImplFinder struct {
	prog *ssa.Program
	memo map[implKey][]*ssa.Function
}

// NewImplFinder returns a finder over prog. prog may be nil (Of then returns nil).
func NewImplFinder(prog *ssa.Program) *ImplFinder {
	return &ImplFinder{prog: prog, memo: map[implKey][]*ssa.Function{}}
}

// Of resolves common's interface method to its concrete implementations, memoised. Returns
// nil for a non-invoke call (no Method) or when the receiver type is not an interface.
func (f *ImplFinder) Of(common *ssa.CallCommon) []*ssa.Function {
	meth := common.Method
	if meth == nil || f.prog == nil {
		return nil
	}
	iface, ok := common.Value.Type().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	k := implKey{iface: iface.String(), meth: meth}
	if v, ok := f.memo[k]; ok {
		return v
	}
	v := implementationsOver(f.prog, iface, meth)
	f.memo[k] = v
	return v
}

// freeVarLoadInvoked reports whether loading the idx-th free var of fn (a *T captured by
// reference) yields a func value that is invoked within fn.
func (p *txProbe) freeVarLoadInvoked(fn *ssa.Function, idx int) bool {
	fv := fn.FreeVars[idx]
	for _, ref := range referrers(fv) {
		if u, ok := ref.(*ssa.UnOp); ok && u.Op == token.MUL && p.valueInvoked(u, fn) {
			return true
		}
	}
	return false
}

func bindingIndex(mc *ssa.MakeClosure, v ssa.Value) int {
	for i, b := range mc.Bindings {
		if b == v {
			return i
		}
	}
	return -1
}

// referrers returns v's referrer instructions (nil-safe).
func referrers(v ssa.Value) []ssa.Instruction {
	r := v.Referrers()
	if r == nil {
		return nil
	}
	return *r
}

// ProgOf returns the SSA program the result's call-graph nodes belong to (every node
// shares one program), or nil for an empty graph. Shared so the reclaim and rebind passes
// build their ImplFinder over the same program.
func ProgOf(res *analyze.Result) *ssa.Program { return progOf(res) }

func progOf(res *analyze.Result) *ssa.Program {
	for _, n := range res.Graph.Nodes {
		if n.Func != nil {
			return n.Func.Prog
		}
	}
	return nil
}

func anyFlowsTo(recvs []ssa.Value, target *ssa.Function) bool {
	for _, r := range recvs {
		if flowsTo(r, target, map[ssa.Value]bool{}) {
			return true
		}
	}
	return false
}

// flowsTo reports whether v derives from the closure target, following the value
// constructors the strict-server idiom puts between the closure and the
// http.Handler the ServeHTTP receiver holds: a direct *ssa.Function reference,
// MakeClosure(target), MakeInterface, ChangeType/Convert (the http.HandlerFunc
// conversion), and Phi (the middleware loop's pre-loop operand is the
// target-derived handler). It deliberately does NOT trace into Calls — that would
// be unsound to assume and is unnecessary, since the Phi's pre-loop operand already
// carries the closure. Bounded by a visited set.
func flowsTo(v ssa.Value, target *ssa.Function, seen map[ssa.Value]bool) bool {
	if v == nil || seen[v] {
		return false
	}
	seen[v] = true
	switch x := v.(type) {
	case *ssa.Function:
		return x == target
	case *ssa.MakeClosure:
		fn, _ := x.Fn.(*ssa.Function)
		return fn == target
	case *ssa.MakeInterface:
		return flowsTo(x.X, target, seen)
	case *ssa.ChangeType:
		return flowsTo(x.X, target, seen)
	case *ssa.Convert:
		return flowsTo(x.X, target, seen)
	case *ssa.Phi:
		for _, e := range x.Edges {
			if flowsTo(e, target, seen) {
				return true
			}
		}
	}
	return false
}
