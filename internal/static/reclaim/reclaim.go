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
					closure := closureFn(arg)
					if closure == nil {
						continue
					}
					if !newTxProbe().invokesParam(callee, i) {
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

// closureFn returns the anonymous function behind a closure value passed as a call
// argument — a *ssa.MakeClosure, optionally behind the type conversions a func-value
// argument can carry — or nil if the argument is not a closure. It deliberately matches
// only MakeClosure (the orphaned-closure case), not a bare *ssa.Function: a top-level
// func passed as a value is address-taken and already resolved by the call-graph builder.
func closureFn(v ssa.Value) *ssa.Function {
	switch x := v.(type) {
	case *ssa.MakeClosure:
		fn, _ := x.Fn.(*ssa.Function)
		return fn
	case *ssa.ChangeType:
		return closureFn(x.X)
	case *ssa.Convert:
		return closureFn(x.X)
	case *ssa.MakeInterface:
		return closureFn(x.X)
	}
	return nil
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
// result a pure function of the SSA, so TxClosure is deterministic.
type txProbe struct {
	param map[invKey]bool
	val   map[ssa.Value]bool
}

func newTxProbe() *txProbe {
	return &txProbe{param: map[invKey]bool{}, val: map[ssa.Value]bool{}}
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
			if callee := common.StaticCallee(); callee != nil {
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
