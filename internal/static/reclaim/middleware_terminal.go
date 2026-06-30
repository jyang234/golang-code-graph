package reclaim

import (
	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// recoverTerminals recovers the edge(s) to the business handler the middleware chain
// dispatches to, and reports whether EVERY terminal the loop feeds was recovered (the gate
// the empty-set blind-spot clearing rides on). Three shapes:
//
//   - INLINE (http): f dispatches `h.ServeHTTP(...)` on the threaded handler itself. The
//     handler is built in f, so its target is f's own initial handler — recover f→T.
//   - INLINE (strict-server): f dispatches the threaded handler by CALLING it as a func value
//     `h(ctx, w, r, request)` — the oapi-codegen strict layer, whose handler is a plain func
//     type, not an http.Handler. Same recovery as the http inline shape: f→T, T being f's
//     initial handler (the per-operation closure).
//   - FACTORED: f RETURNS the threaded handler and the CALLER dispatches
//     `f(handler).ServeHTTP(...)`. The target is the handler the caller passed in — recover
//     caller→T, traced to the caller's argument at the handler parameter's position.
//
// Returns false when a terminal cannot be resolved to a concrete handler function, or (for
// the factored shape) when a caller uses f's returned handler in any way other than a
// recovered ServeHTTP dispatch — either case leaves a hop unaccounted, so the seam must not
// be cleared.
func (r *mwReclaimer) recoverTerminals(fqn string, f *ssa.Function, lp mwLoop, addEdge func(from, to string)) bool {
	// INLINE (http): a ServeHTTP receiver in f that derives from the threaded handler.
	for _, recv := range serveHTTPReceivers(f) {
		if valueReaches(recv, lp.phi, map[ssa.Value]bool{}) {
			t := handlerTarget(lp.initial)
			if t == nil {
				return false
			}
			addEdge(fqn, t.RelString(nil))
			return true
		}
	}
	// INLINE (strict-server): f calls the threaded handler as a func value `h(...)`.
	if dispatchesThreadedHandler(f, lp) {
		t := handlerTarget(lp.initial)
		if t == nil {
			return false
		}
		addEdge(fqn, t.RelString(nil))
		return true
	}
	// FACTORED: f returns the threaded handler; the caller dispatches its ServeHTTP. EVERY
	// return must yield the threaded handler — a sibling return of a DIFFERENT handler is an
	// alternate terminal the caller could dispatch, so binding the terminal solely from the
	// handler argument would miss that path (and clearing the seam on top would launder it into
	// a false absence proof). If any return does not yield the threaded handler, abstain.
	if !everyReturnThreaded(f, lp.phi) {
		return false
	}
	return r.recoverFactoredTerminals(f, lp, addEdge)
}

// recoverFactoredTerminals handles the factored shape: for every caller that dispatches
// ServeHTTP on f's returned handler, recover caller→T. Returns false if any caller uses the
// returned handler in a way other than a ServeHTTP dispatch this pass can resolve — that
// handler then flows somewhere unaccounted, so the empty-set seam must stay disclosed.
func (r *mwReclaimer) recoverFactoredTerminals(f *ssa.Function, lp mwLoop, addEdge func(from, to string)) bool {
	paramIdx, isParam := paramIndex(f, lp.initial)
	allRecovered := true
	for _, cs := range r.callersOf(f) {
		result := ssa.Value(cs.call)
		// Every use of f's returned handler must be a ServeHTTP dispatch we resolve;
		// any other referrer means it escapes into an untraced hop.
		for _, ref := range referrers(result) {
			if !isServeHTTPReceiverOf(ref, result) {
				allRecovered = false
				continue
			}
			t := r.factoredTarget(lp, cs.call, paramIdx, isParam)
			if t == nil {
				allRecovered = false
				continue
			}
			addEdge(cs.callerFQN, t.RelString(nil))
		}
	}
	return allRecovered
}

// dispatchesThreadedHandler reports whether f invokes the threaded handler by CALLING it as a
// func value — the oapi-codegen strict-server terminal `h(ctx, w, r, request)`, distinct from
// the http layer's `h.ServeHTTP(...)`. It matches a func-value call (not an interface or
// static call, not a builtin) whose CALLEE value reaches the loop phi. The loop's own
// `mw(h …)` call is excluded both explicitly (it is lp.call) and intrinsically (its callee is
// a slice element, not the phi). recoverTerminals runs for EVERY loop, empty or not, so this
// fires on both: the recovered f→initial edge is a sound MAY edge in either case — when the set
// is empty the loop body is dead and `h` is exactly f's initial handler (invoked directly);
// when non-empty `h` is the wrapped handler but the initial handler is still reached through
// the chain, so f can-reach it. Only the empty case gates seam clearing — that gate lives in
// reclaimFunc (`len(set) == 0`), not here.
func dispatchesThreadedHandler(f *ssa.Function, lp mwLoop) bool {
	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok || call == lp.call {
				continue
			}
			c := call.Common()
			if !isFuncValueCall(c) {
				continue
			}
			if valueReaches(c.Value, lp.phi, map[ssa.Value]bool{}) {
				return true
			}
		}
	}
	return false
}

// isServeHTTPReceiverOf reports whether instr is a net/http ServeHTTP invoke whose receiver
// is exactly result. It shares the one net/http-ServeHTTP predicate (isServeHTTPInvoke) with
// serveHTTPReceivers, so the soundness-load-bearing matcher cannot drift between them.
func isServeHTTPReceiverOf(instr ssa.Instruction, result ssa.Value) bool {
	call, ok := instr.(ssa.CallInstruction)
	if !ok {
		return false
	}
	c := call.Common()
	return isServeHTTPInvoke(c) && c.Value == result
}

// factoredTarget resolves the business handler a factored chain dispatches to. When the
// loop's initial handler is concrete inside f (unusual), that is the target; otherwise it is
// the handler parameter, whose concrete value is the caller's argument at the matching index.
func (r *mwReclaimer) factoredTarget(lp mwLoop, cs *ssa.Call, paramIdx int, isParam bool) *ssa.Function {
	if t := handlerTarget(lp.initial); t != nil {
		return t
	}
	if !isParam {
		return nil
	}
	arg := callArg(cs, paramIdx)
	if arg == nil {
		return nil
	}
	return handlerTarget(arg)
}

// hasUnresolvedFuncCallOfType reports whether f contains a func-value call of element type
// typeName that is NOT one of the recognized middleware-loop calls — an unresolved seam of
// the same type whose blind spot must survive even though the loops cleared. Without this
// guard, clearing the loop's (Site, Type) blind spot would also silently drop the unrelated
// call's disclosure.
func (r *mwReclaimer) hasUnresolvedFuncCallOfType(f *ssa.Function, loops []mwLoop, typeName string) bool {
	loopCalls := map[*ssa.Call]bool{}
	for _, lp := range loops {
		loopCalls[lp.call] = true
	}
	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok || loopCalls[call] {
				continue
			}
			c := call.Common()
			if !isFuncValueCall(c) {
				continue
			}
			// One source of truth: the same blindspots.FuncValueTypeName the seam TypeName and
			// the blind-spot Detail are built from, so the producer's exact-equality guard and
			// the disclosure agree on the type string.
			if blindspots.FuncValueTypeName(c.Value.Type()) == typeName {
				return true
			}
		}
	}
	return false
}

// valueReaches reports whether v derives from target through the value constructors a
// handler flows through between the threaded phi (or a call result) and a ServeHTTP
// receiver: identity, the http.HandlerFunc/MiddlewareFunc conversions, MakeInterface, and
// the loop phi itself. Bounded by a visited set.
func valueReaches(v, target ssa.Value, seen map[ssa.Value]bool) bool {
	if v == nil || seen[v] {
		return false
	}
	if v == target {
		return true
	}
	seen[v] = true
	switch x := v.(type) {
	case *ssa.MakeInterface:
		return valueReaches(x.X, target, seen)
	case *ssa.ChangeType:
		return valueReaches(x.X, target, seen)
	case *ssa.Convert:
		return valueReaches(x.X, target, seen)
	case *ssa.Phi:
		for _, e := range x.Edges {
			if valueReaches(e, target, seen) {
				return true
			}
		}
	}
	return false
}

// everyReturnThreaded reports whether f returns the threaded handler on EVERY return path:
// there is at least one return, and every return has a result that derives from target (the
// threaded handler phi). This is the factored shape where f's SOLE terminal is the wrapped
// handler — so the caller's ServeHTTP on f's result always dispatches that handler. A return
// that yields no threaded result (a sibling `return otherHandler`, a post-loop re-wrap whose
// value is a Call valueReaches cannot trace, or a void return) means f has an alternate
// terminal this pass cannot bind, so it returns false and the caller abstains.
func everyReturnThreaded(f *ssa.Function, target ssa.Value) bool {
	sawThreaded := false
	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			threaded := false
			for _, res := range ret.Results {
				if valueReaches(res, target, map[ssa.Value]bool{}) {
					threaded = true
					break
				}
			}
			if !threaded {
				return false
			}
			sawThreaded = true
		}
	}
	return sawThreaded
}

// paramIndex returns the index of v among f's parameters and whether v is a parameter at
// all. The index is into f.Params (the receiver, for a method, is Params[0]).
func paramIndex(f *ssa.Function, v ssa.Value) (int, bool) {
	p, ok := v.(*ssa.Parameter)
	if !ok {
		return 0, false
	}
	for i, fp := range f.Params {
		if fp == p {
			return i, true
		}
	}
	return 0, false
}

// callersOf returns every static call site of f across the graph (the caller's FQN and the
// call instruction). The index is built lazily on first use from ONE sweep of the graph
// nodes' SSA and shared across the pass, so resolving many factored middleware loops costs a
// single sweep rather than re-scanning every node per loop. Built in res.Graph.Nodes order
// (sorted by FQN) then SSA block/instr order, so callersOf(f) is deterministic.
func (r *mwReclaimer) callersOf(f *ssa.Function) []mwCallSite {
	if r.callerSites == nil {
		r.callerSites = map[*ssa.Function][]mwCallSite{}
		for _, n := range r.res.Graph.Nodes {
			caller := n.Func
			if caller == nil {
				continue
			}
			for _, b := range caller.Blocks {
				for _, instr := range b.Instrs {
					call, ok := instr.(*ssa.Call)
					if !ok {
						continue
					}
					if callee := call.Common().StaticCallee(); callee != nil {
						r.callerSites[callee] = append(r.callerSites[callee], mwCallSite{callerFQN: n.FQN, call: call})
					}
				}
			}
		}
	}
	return r.callerSites[f]
}

// callArg returns the call argument at the given parameter index, accounting for whether
// the static call's Args slice includes the receiver (a method call) or not. Returns nil
// when the index does not map onto an argument.
func callArg(cs *ssa.Call, paramIdx int) ssa.Value {
	args := cs.Common().Args
	callee := cs.Common().StaticCallee()
	if callee == nil {
		return nil
	}
	// Args carries one entry per parameter; the receiver, when present, aligns with
	// Params[0]. When Args is one shorter than Params the receiver is omitted, so shift.
	idx := paramIdx
	if len(args) == len(callee.Params)-1 {
		idx = paramIdx - 1
	}
	if idx < 0 || idx >= len(args) {
		return nil
	}
	return args[idx]
}
