package reclaim

import (
	"go/types"

	"golang.org/x/tools/go/ssa"
)

// recoverTerminals recovers the edge(s) to the business handler the middleware chain
// dispatches to, and reports whether EVERY terminal the loop feeds was recovered (the gate
// the empty-set blind-spot clearing rides on). Two shapes:
//
//   - INLINE: f dispatches `h.ServeHTTP(...)` on the threaded handler itself. The handler is
//     built in f, so its target is f's own initial handler — recover f→T.
//   - FACTORED: f RETURNS the threaded handler and the CALLER dispatches
//     `f(handler).ServeHTTP(...)`. The target is the handler the caller passed in — recover
//     caller→T, traced to the caller's argument at the handler parameter's position.
//
// Returns false when a terminal cannot be resolved to a concrete handler function, or (for
// the factored shape) when a caller uses f's returned handler in any way other than a
// recovered ServeHTTP dispatch — either case leaves a hop unaccounted, so the seam must not
// be cleared.
func (r *mwReclaimer) recoverTerminals(fqn string, f *ssa.Function, lp mwLoop, addEdge func(from, to string)) bool {
	// INLINE: a ServeHTTP receiver in f that derives from the threaded handler.
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
	// FACTORED: f returns the threaded handler; the caller dispatches its ServeHTTP.
	if !functionReturns(f, lp.phi) {
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
	for _, n := range r.res.Graph.Nodes {
		caller := n.Func
		if caller == nil {
			continue
		}
		for _, cs := range callSitesTo(caller, f) {
			result := ssa.Value(cs)
			// Every use of f's returned handler must be a ServeHTTP dispatch we resolve;
			// any other referrer means it escapes into an untraced hop.
			for _, ref := range referrers(result) {
				if !isServeHTTPReceiverOf(ref, result) {
					allRecovered = false
					continue
				}
				t := r.factoredTarget(lp, cs, paramIdx, isParam)
				if t == nil {
					allRecovered = false
					continue
				}
				addEdge(n.FQN, t.RelString(nil))
			}
		}
	}
	return allRecovered
}

// isServeHTTPReceiverOf reports whether instr is a net/http ServeHTTP invoke whose receiver
// is exactly result. The method is matched by net/http package + name (not the bare name
// "ServeHTTP") for the same reason serveHTTPReceivers matches it: the chain's soundness
// rests on http.HandlerFunc.ServeHTTP invoking the underlying handler.
func isServeHTTPReceiverOf(instr ssa.Instruction, result ssa.Value) bool {
	call, ok := instr.(ssa.CallInstruction)
	if !ok {
		return false
	}
	c := call.Common()
	return c.IsInvoke() && c.Method != nil && c.Method.Name() == "ServeHTTP" &&
		c.Method.Pkg() != nil && c.Method.Pkg().Path() == "net/http" && c.Value == result
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
			if c.IsInvoke() || c.StaticCallee() != nil {
				continue
			}
			if _, isBuiltin := c.Value.(*ssa.Builtin); isBuiltin {
				continue
			}
			if elemTypeName(c.Value.Type()) == typeName {
				return true
			}
		}
	}
	return false
}

// elemTypeName names a func-value's defined type the way the blind-spot detector does
// (blindspots.funcValueTypeName): the unaliased named type's String(), so the MiddlewareSeam
// TypeName matches the type named in the blind spot's Detail.
func elemTypeName(t types.Type) string {
	if named, ok := types.Unalias(t).(*types.Named); ok {
		return named.String()
	}
	return t.String()
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

// functionReturns reports whether f has a return statement whose returned value derives
// from target (the threaded handler phi) — the factored shape where f hands the wrapped
// handler back to its caller.
func functionReturns(f *ssa.Function, target ssa.Value) bool {
	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			ret, ok := instr.(*ssa.Return)
			if !ok {
				continue
			}
			for _, res := range ret.Results {
				if valueReaches(res, target, map[ssa.Value]bool{}) {
					return true
				}
			}
		}
	}
	return false
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

// callSitesTo returns the static call instructions in caller that invoke callee.
func callSitesTo(caller, callee *ssa.Function) []*ssa.Call {
	var out []*ssa.Call
	for _, b := range caller.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if ok && call.Common().StaticCallee() == callee {
				out = append(out, call)
			}
		}
	}
	return out
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
