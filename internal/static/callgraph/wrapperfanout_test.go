package callgraph_test

// Investigation (D-CX10): does VTA shrink the HighFanOut blind spot a shared
// HTTP middleware creates? The field's lifts abstain at a chi/oapi wrapper
// whose `next.ServeHTTP` RTA-resolves to every handler. This measures, on a
// faithful reproduction, whether VTA prunes that site — the empirical fact
// that decides between "expose/auto-select VTA" and "abstention is the sound
// end state, a bespoke router-aware pass is the only lever."

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// sharedMiddlewareSrc models the oapi/chi shape: ONE middleware type whose
// Serve calls next.Serve, applied to all N handlers (next legitimately ranges
// over every handler). genericRoutes returns the wrapped chain per route.
const sharedMiddlewareSrc = `package fix

type Handler interface{ Serve() }

type H0 struct{}
type H1 struct{}
type H2 struct{}
type H3 struct{}
type H4 struct{}
type H5 struct{}
type H6 struct{}
type H7 struct{}
type H8 struct{}
type H9 struct{}

func sink(int) {}

func (H0) Serve() { sink(0) }
func (H1) Serve() { sink(1) }
func (H2) Serve() { sink(2) }
func (H3) Serve() { sink(3) }
func (H4) Serve() { sink(4) }
func (H5) Serve() { sink(5) }
func (H6) Serve() { sink(6) }
func (H7) Serve() { sink(7) }
func (H8) Serve() { sink(8) }
func (H9) Serve() { sink(9) }

// The shared middleware: one type, wraps every handler.
type mw struct{ next Handler }

func (m mw) Serve() { m.next.Serve() } // THE fan-out site

func wrap(next Handler) Handler { return mw{next: next} }

func main() {
	routes := []Handler{
		wrap(H0{}), wrap(H1{}), wrap(H2{}), wrap(H3{}), wrap(H4{}),
		wrap(H5{}), wrap(H6{}), wrap(H7{}), wrap(H8{}), wrap(H9{}),
	}
	for _, r := range routes {
		r.Serve()
	}
}
`

func buildExperiment(t *testing.T, src string) (*ssa.Program, []*ssa.Function) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fix.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg := types.NewPackage("example.com/fix", "")
	spkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()}, fset, pkg, []*ast.File{f},
		ssa.SanityCheckFunctions|ssa.InstantiateGenerics)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	prog := spkg.Prog
	var roots []*ssa.Function
	if m := spkg.Func("main"); m != nil {
		roots = append(roots, m)
	}
	if init := spkg.Func("init"); init != nil {
		roots = append(roots, init)
	}
	return prog, roots
}

// fanoutAt counts the distinct callees the graph resolves for the invoke call
// to methodName inside fnName — the per-site fan-out the blind-spot detector
// would see.
func fanoutAt(g *callgraph.Graph, fnName, methodName string) int {
	for fn, node := range g.Nodes {
		if fn == nil || fn.Name() != fnName {
			continue
		}
		callees := map[*ssa.Function]bool{}
		for _, e := range node.Out {
			if e.Site == nil {
				continue
			}
			if c := e.Site.Common(); c.IsInvoke() && c.Method != nil && c.Method.Name() == methodName {
				callees[e.Callee.Func] = true
			}
		}
		if len(callees) > 0 {
			return len(callees)
		}
	}
	return 0
}

func TestVTAvsRTAWrapperFanout(t *testing.T) {
	prog, roots := buildExperiment(t, sharedMiddlewareSrc)
	prog.Build()

	rtaRes := rta.Analyze(roots, true)
	rtaFan := fanoutAt(rtaRes.CallGraph, "Serve", "Serve")

	reachable := map[*ssa.Function]bool{}
	for fn := range rtaRes.CallGraph.Nodes {
		if fn != nil {
			reachable[fn] = true
		}
	}
	vtaG := vta.CallGraph(reachable, rtaRes.CallGraph)
	vtaFan := fanoutAt(vtaG, "Serve", "Serve")

	t.Logf("shared-middleware next.Serve() fan-out: RTA=%d VTA=%d (10 handlers wrapped by one mw)", rtaFan, vtaFan)

	// The measurement is the deliverable; these bounds make the finding a
	// committed fact rather than a log line. Whatever the numbers, next
	// genuinely ranges over all 10 handlers at this shared site, so neither
	// algorithm can soundly prune below 10 — the over-approximation is the
	// program's actual dynamic behavior, not an analysis artifact.
	if rtaFan < 10 {
		t.Errorf("RTA fan-out %d < 10: a shared middleware's next must reach every wrapped handler", rtaFan)
	}
	if vtaFan < 10 {
		t.Errorf("VTA fan-out %d < 10: VTA unsoundly pruned a site whose next really does range over all handlers", vtaFan)
	}
}
