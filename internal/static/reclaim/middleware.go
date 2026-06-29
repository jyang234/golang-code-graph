package reclaim

import (
	"go/token"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
)

// ViaMiddlewareChain attributes an edge to the middleware-chain reclaimer.
const ViaMiddlewareChain = "middleware-chain"

// MiddlewareSeam names a middleware-application loop the reclaimer fully resolved
// to an EMPTY middleware set, so the UnresolvedCall blind spot the loop produced
// can be dropped. Site is the loop function's FQN (it matches blindspots.BlindSpot.Site,
// which is fn.RelString(nil)); TypeName is the MiddlewareFunc element type's String()
// (it matches the type named in the blind spot's Detail). The apply pass drops a
// blind spot only when BOTH match, so a same-function UnresolvedCall of a different
// func type is never silently cleared.
type MiddlewareSeam struct {
	Site     string
	TypeName string
}

// MiddlewareResult is the middleware-chain reclaimer's output: the recovered edges
// (provenance via=middleware-chain) and the loops whose seam is fully resolved to an
// empty set (so the blind spot may be dropped). Edges and ResolvedEmpty are deterministic:
// the iteration order follows res.Graph.Nodes (sorted by FQN) and the SSA block/instr order.
type MiddlewareResult struct {
	Edges         []Edge
	ResolvedEmpty []MiddlewareSeam
}

// MiddlewareChain reclaims the middleware-application loop that oapi-codegen servers and
// hand-written chi/net-http stacks apply at every route:
//
//	for _, mw := range siw.HandlerMiddlewares { h = mw(h) }; h.ServeHTTP(w, r)
//
// The `mw(h)` call is a func-value call the builder resolves to no callee (an
// UnresolvedCall blind spot), so any entrypoint-anchored absence proof whose reachable
// cone crosses the loop abstains (CANT-PROVE). This pass recovers the edges that loop
// implies WHEN the middleware element set is statically provable, and discloses honestly
// where it is not.
//
// What it recovers, per recognized loop in function F (slice S threading handler H via
// `H = mw(H)`, with the chain dispatched through H.ServeHTTP):
//
//  1. F→Mi for every middleware function Mi in S — the loop invokes each (resolving the
//     `mw(h)` call site to its concrete callees). Emitted only when S's element set is
//     PROVABLY COMPLETE (a const slice/array literal, an append chain of known funcs, or
//     provably empty); a slice built from an unknown source (a parameter, an opaque load)
//     leaves the seam UnresolvedCall — abstaining is correct, a guessed edge would be a
//     false PROVEN (tenet 4).
//  2. The terminal handler edge — the business handler whose ServeHTTP terminates the
//     chain CAN be invoked when a request flows through, so the route reaches it. For the
//     INLINE shape (ServeHTTP in F, on the threaded handler) that is F→T; for the FACTORED
//     shape (F returns the handler and the CALLER dispatches `F(h).ServeHTTP(...)`) it is
//     caller→T, traced to the handler argument the caller passed. T is the concrete func a
//     net/http.HandlerFunc / closure handler wraps.
//
// Soundness (R2): every edge is one real execution can take — adding a true edge only ever
// turns provenAbsent→reachable, never the reverse. The terminal edge is a MAY edge (a
// middleware may short-circuit and never call next), which is exactly R2's "can take".
//
// Blind-spot clearing: the loop's UnresolvedCall is dropped (ResolvedEmpty) ONLY when the
// set is provably EMPTY and every terminal handler the loop feeds was recovered. An empty
// set means the loop body is dead — it hides no middleware — so once the pass-through
// handler edge is recovered the loop's frontier is fully resolved. A NON-empty set is NOT
// cleared: each middleware body re-dispatches through its own next.ServeHTTP, a residual
// seam this pass does not chase, so it stays disclosed (the strict-server reclaimer's
// reconnect-but-disclose discipline). Over-clearing would launder those hidden hops into a
// false absence proof.
func MiddlewareChain(res *analyze.Result) MiddlewareResult {
	prog := progOf(res)
	if prog == nil {
		return MiddlewareResult{}
	}
	var out MiddlewareResult
	seen := map[[2]string]bool{}
	addEdge := func(from, to string) {
		if from == "" || to == "" {
			return
		}
		key := [2]string{from, to}
		if seen[key] {
			return
		}
		seen[key] = true
		out.Edges = append(out.Edges, Edge{From: from, To: to, Via: ViaMiddlewareChain})
	}
	r := &mwReclaimer{res: res, prog: prog, fieldMemo: map[*types.Var]fieldSet{}}
	for _, n := range res.Graph.Nodes {
		f := n.Func
		if f == nil {
			continue
		}
		r.reclaimFunc(n.FQN, f, addEdge, &out)
	}
	// reclaimFunc emits ResolvedEmpty in a per-function map-iteration order (the type set);
	// sort on (Site, TypeName) so the cleared-seam set is byte-identical across runs.
	sort.Slice(out.ResolvedEmpty, func(i, j int) bool {
		if out.ResolvedEmpty[i].Site != out.ResolvedEmpty[j].Site {
			return out.ResolvedEmpty[i].Site < out.ResolvedEmpty[j].Site
		}
		return out.ResolvedEmpty[i].TypeName < out.ResolvedEmpty[j].TypeName
	})
	return out
}

// mwReclaimer carries the cross-call state of one MiddlewareChain pass: the analyzed
// result, its program, and a memo for the call-site-independent field-element resolution
// so a field referenced by many route methods is resolved once. A reclaimer is single-pass
// and not safe for concurrent use; MiddlewareChain runs it sequentially and deterministically.
type mwReclaimer struct {
	res       *analyze.Result
	prog      *ssa.Program
	fieldMemo map[*types.Var]fieldSet
}

// mwLoop is one recognized middleware-application loop in a function.
type mwLoop struct {
	call     *ssa.Call  // the `mw(h)` func-value call
	phi      *ssa.Phi   // the threaded handler phi (`h`)
	initial  ssa.Value  // the phi's pre-loop operand (the handler before any middleware)
	slice    ssa.Value  // the slice the middleware elements are ranged from
	elemType types.Type // the func-value callee's defined type (the MiddlewareFunc type)
}

// reclaimFunc finds and reclaims every middleware-application loop in f, appending the
// recovered edges via addEdge and any fully-resolved empty seam to out.ResolvedEmpty.
func (r *mwReclaimer) reclaimFunc(fqn string, f *ssa.Function, addEdge func(from, to string), out *MiddlewareResult) {
	loops := findMiddlewareLoops(f)
	if len(loops) == 0 {
		return
	}
	// clearableByType[T] stays true only while every loop of element type T in f resolves
	// to an empty set with all terminals recovered; it flips false on the first loop of T
	// that does not. A type with no surviving clearable loop is dropped, so its seam stays
	// disclosed.
	clearableByType := map[string]bool{}
	seenType := map[string]bool{}
	for _, lp := range loops {
		typeName := lp.elemType.String()
		if !seenType[typeName] {
			seenType[typeName] = true
			clearableByType[typeName] = true
		}
		set, ok := r.resolveSet(lp.slice)
		if !ok {
			clearableByType[typeName] = false // abstain: leave the seam blind
			continue
		}
		for _, mi := range set {
			addEdge(fqn, mi.RelString(nil))
		}
		terminalsOK := r.recoverTerminals(fqn, f, lp, addEdge)
		if len(set) != 0 || !terminalsOK {
			clearableByType[typeName] = false
		}
	}
	// A seam is clearable only if EVERY func-value call of its element type in f is a
	// resolved-empty middleware loop — otherwise an unresolved same-type call in f would be
	// silently dropped along with the loop's blind spot.
	for typeName, clearable := range clearableByType {
		if clearable && !r.hasUnresolvedFuncCallOfType(f, loops, typeName) {
			out.ResolvedEmpty = append(out.ResolvedEmpty, MiddlewareSeam{Site: fqn, TypeName: typeName})
		}
	}
}

// findMiddlewareLoops returns every middleware-application loop in f: a func-value call
// `c = mw(h)` whose callee is a slice element and whose handler argument is a phi fed back
// by the call's own result (the `h = mw(h)` recurrence). That recurrence is the signature
// that distinguishes a middleware chain from an ordinary "call each func in a slice" loop
// (where the argument is loop-invariant), and it is what lets the terminal handler be traced.
func findMiddlewareLoops(f *ssa.Function) []mwLoop {
	var out []mwLoop
	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(*ssa.Call)
			if !ok {
				continue
			}
			c := call.Common()
			if c.IsInvoke() || c.StaticCallee() != nil {
				continue // a resolved or interface call, not a func-value seam
			}
			slice, ok := sliceElementCallee(c.Value)
			if !ok {
				continue
			}
			phi, initial, ok := threadedHandler(call, c.Args)
			if !ok {
				continue
			}
			out = append(out, mwLoop{
				call:     call,
				phi:      phi,
				initial:  initial,
				slice:    slice,
				elemType: c.Value.Type(),
			})
		}
	}
	return out
}

// sliceElementCallee reports whether callee is a value loaded from a slice element
// (`*ssa.UnOp(MUL)` of `*ssa.IndexAddr`), returning the underlying slice value. This is the
// `mw` of `mw := slice[i]` produced by `for _, mw := range slice`.
func sliceElementCallee(callee ssa.Value) (ssa.Value, bool) {
	load, ok := callee.(*ssa.UnOp)
	if !ok || load.Op != token.MUL {
		return nil, false
	}
	idx, ok := load.X.(*ssa.IndexAddr)
	if !ok {
		return nil, false
	}
	return idx.X, true
}

// threadedHandler returns the handler phi `h` of a `h = mw(h)` call and the phi's pre-loop
// operand (the handler before any middleware runs). The phi is the argument whose own
// edges include call (the call result loops back into it); the pre-loop operand is the
// other edge. Returns ok=false if the call's argument is not such a self-threading phi.
func threadedHandler(call *ssa.Call, args []ssa.Value) (*ssa.Phi, ssa.Value, bool) {
	if len(args) != 1 {
		return nil, nil, false
	}
	phi, ok := args[0].(*ssa.Phi)
	if !ok {
		return nil, nil, false
	}
	var initial ssa.Value
	loops := false
	for _, e := range phi.Edges {
		if e == ssa.Value(call) {
			loops = true
			continue
		}
		initial = e
	}
	if !loops || initial == nil {
		return nil, nil, false
	}
	return phi, initial, true
}
