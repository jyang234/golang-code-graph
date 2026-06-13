package obligations

// Evidence (D-CX10): the field's lifts abstain at the chi/oapi wrapper, and
// NEITHER precision lever proposed in the investigation fixes it. This locks
// why, so the roadmap is not re-derived on a refuted hypothesis:
//
//   - a shared middleware (mw.Serve calling next.Serve, where next resolves
//     back to mw) is a self-referential SCC; entry domination abstains there
//     by the recursion guard (D-CX1), under BOTH RTA and VTA — VTA does not
//     break the self-loop in the engine's resolution, so it is not the lever;
//   - the real oapi/chi shape stores handlers as func VALUES in a router, which
//     takes their address; entry domination abstains by the address-taken
//     guard — a genuine soundness boundary (the handler can be invoked from
//     framework code the unit cannot see), which no call-graph precision fixes.
//
// Both abstentions are correct. The lifts deliver on statically-resolved cones
// (the field's orchestrator took every derived row); across a framework
// dispatch boundary, honest abstention is the end state, and the lever is
// rule anchoring (keep require and before on the same resolved side), not a
// deeper call graph. See docs/design/wrapper-fanout-investigation.md.

import (
	"testing"

	xcg "golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/ssa"
)

const wrapperIfaceSrc = `package fix

func Validate() error { return nil }
func Publish()        {}

type Handler interface{ Serve() }

type PubH struct{}
func (PubH) Serve() { Publish() }

type A struct{}; func (A) Serve() {}
type B struct{}; func (B) Serve() {}
type C struct{}; func (C) Serve() {}

type mw struct{ next Handler }
func (m mw) Serve() { m.next.Serve() }
func wrap(n Handler) Handler { return mw{next: n} }

func doPublish(h Handler) {
	if Validate() == nil {
		h.Serve()
	}
}

func main() {
	for _, h := range []Handler{ wrap(PubH{}), wrap(A{}), wrap(B{}), wrap(C{}) } {
		doPublish(h)
	}
}
`

const wrapperFuncRegistrySrc = `package fix

func Validate() error { return nil }
func Publish()        {}

func pubHandler() { Publish() }
func aHandler()   {}
func bHandler()   {}

type router struct{ routes []func() }
func (r *router) handle(h func()) { r.routes = append(r.routes, h) }
func (r *router) serve()          { for _, h := range r.routes { h() } }

func doPublish(r *router) {
	if Validate() == nil {
		r.serve()
	}
}

func main() {
	r := &router{}
	r.handle(pubHandler)
	r.handle(aHandler)
	r.handle(bHandler)
	doPublish(r)
}
`

func unitFromCallgraph(g *xcg.Graph) *Unit {
	var fns []*ssa.Function
	bySite := map[ssa.CallInstruction][]*ssa.Function{}
	for fn, node := range g.Nodes {
		if fn == nil {
			continue
		}
		fns = append(fns, fn)
		for _, e := range node.Out {
			if e.Site != nil && e.Callee.Func != nil {
				bySite[e.Site] = append(bySite[e.Site], e.Callee.Func)
			}
		}
	}
	return &Unit{Fns: fns, Callees: func(s ssa.CallInstruction) []*ssa.Function { return bySite[s] }}
}

func callerOfPublish(fns []*ssa.Function, name string) *ssa.Function {
	for _, fn := range fns {
		if name != "" && fn.Name() != name {
			continue
		}
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if c, ok := in.(*ssa.Call); ok {
					if sc := c.Common().StaticCallee(); sc != nil && sc.Name() == "Publish" {
						return fn
					}
				}
			}
		}
	}
	return nil
}

func mainRoots(fns []*ssa.Function) []*ssa.Function {
	var roots []*ssa.Function
	for _, fn := range fns {
		if fn.Parent() == nil && (fn.Name() == "main" || fn.Name() == "init") && fn.Signature.Recv() == nil {
			roots = append(roots, fn)
		}
	}
	return roots
}

// TestWrapperAbstentionIsFundamental: under both RTA and VTA, the must-precede
// lift abstains on the wrapper patterns — the precision levers do not unlock it.
func TestWrapperAbstentionIsFundamental(t *testing.T) {
	cases := []struct {
		name, src, callerName string
	}{
		{"shared-middleware-SCC", wrapperIfaceSrc, "Serve"},
		{"func-registry-addr-taken", wrapperFuncRegistrySrc, "pubHandler"},
	}
	for _, c := range cases {
		fns := buildProg(t, c.src)
		fns[0].Prog.Build()
		rtaG := rta.Analyze(mainRoots(fns), true).CallGraph
		reach := map[*ssa.Function]bool{}
		for fn := range rtaG.Nodes {
			if fn != nil {
				reach[fn] = true
			}
		}
		vtaG := vta.CallGraph(reach, rtaG)

		for _, algo := range []struct {
			name string
			g    *xcg.Graph
		}{{"RTA", rtaG}, {"VTA", vtaG}} {
			u := unitFromCallgraph(algo.g)
			target := callerOfPublish(u.Fns, c.callerName)
			if target == nil {
				t.Fatalf("%s/%s: target not found", c.name, algo.name)
			}
			sum, note := NewSummaries(u).EntryDominatedNote(target, "example.com/fix#Validate")
			if sum != SummaryUnknown {
				t.Errorf("%s/%s: lift = %s, want UNKNOWN (the abstention is the documented end state); note=%q",
					c.name, algo.name, sum, note)
			}
			t.Logf("%-26s %-4s = %s // %s", c.name, algo.name, sum, note)
		}
	}
}
