// Package reclaim holds the sound static reclaimers from
// docs/design/frontier-instrumentation-plan.md, Phase 3: passes that ADD the
// call edges the builder lost at a recognized framework dispatch seam, shrinking
// the Category-B frontier. Every reclaimer obeys rule R2 — it emits only edges
// real execution can take, so adding them can never manufacture a false proof of
// absence (an added true edge turns provenAbsent→reachable, never the reverse).
// Reclaimers are opt-in (D2) and their edges carry provenance (the Via field).
package reclaim

import (
	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
)

// ViaStrictServer attributes an edge to the strict-server reclaimer.
const ViaStrictServer = "strict-server"

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
