package graphio

import (
	"sort"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	cg "github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
)

// firstPartyScope is the set of all reachable first-party functions, EXCLUDING
// package initializers. init is seeded into RTA so addresses taken during package
// initialization (the registration idiom) resolve dispatch — but init ordering is
// runtime plumbing, not service behavior, so rendering it would add an init node per
// package plus init→init edges to every graph (pure noise that no consumer reads;
// the obligations layer handles init-time obligations separately). The recovered
// dispatch edges are between real route-reachable functions, so excluding init from
// the rendered graph keeps the recovery while leaving the service graph clean. The
// edgeOf init-callee guard is the backstop should init ever re-enter scope.
func firstPartyScope(res *analyze.Result) map[*ssa.Function]bool {
	s := make(map[*ssa.Function]bool)
	for _, n := range res.Graph.Nodes {
		if features.IsPackageInit(n.Func) {
			continue
		}
		// IsFirstPartyFunc, not IsFirstParty(fn.Pkg): go/ssa leaves generic instances
		// and $bound/$thunk method-value wrappers with a nil fn.Pkg, so keying on the
		// package alone drops those reachable first-party functions from the rendered
		// graph — and edgeOf then drops every edge into them as "third-party" with no
		// blind spot, a false "no path" (C-1).
		if res.Program.IsFirstPartyFunc(n.Func) {
			s[n.Func] = true
		}
	}
	return s
}

// rootFuncSet is the set of root functions, for tier purposes (roots are entries).
func rootFuncSet(res *analyze.Result) map[*ssa.Function]bool {
	s := make(map[*ssa.Function]bool)
	for _, r := range res.Roots.Roots {
		s[r.Func] = true
	}
	return s
}

// compositionRoots returns the sorted, deduped import paths of this unit's
// composition-root packages — the defining packages of the KindMain roots
// (ssautil.MainPackages). It is the AUTHORITATIVE main-package set: keyed on the
// root kind the loader assigned, never on an FQN string shape, so a non-main
// package that declares a package-level `func main` is not among them. Empty for a
// unit with no command (a library). The rollup serializes this on the Graph and
// reads it to classify composition-root wiring; see graphio.Graph.CompositionRoots.
func compositionRoots(res *analyze.Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range res.Roots.Roots {
		if r.Kind != roots.KindMain {
			continue
		}
		if pkg := features.PkgPath(r.Func); pkg != "" && !seen[pkg] {
			seen[pkg] = true
			out = append(out, pkg)
		}
	}
	sort.Strings(out)
	return out
}

// omittedPackages returns the sorted, deduped import paths of the first-party
// packages a rendered C3 component IMPORTS but that declare no functions
// (types/consts only) — the imported-but-invisible internal packages the rollup
// discloses (see Graph.OmittedPackages). g supplies the component set (its node
// packages); res supplies the first-party package universe and the direct-import
// graph. Empty (nil) when every imported first-party package contributes a node.
//
// "Has functions" is collected through the COMPLETE walk (ssautil.AllFunctions —
// methods, *T-receiver methods, wrappers, and nested closures), never pkg.Members:
// Members omits all methods, so a types-WITH-methods package would be miscounted as
// function-less and falsely disclosed as omitted (CLAUDE.md: collect functions
// completely). The walk is restricted to first-party paths as it goes, so it records
// only the packages this disclosure can name. A package WITH functions is never
// listed even if all of them are unreachable (no node): that is the frontier/
// missed-root concern, kept distinct so this footnote stays a structural,
// algo-independent "this package has nothing to roll up" rather than a reachability
// claim.
func omittedPackages(res *analyze.Result, g *Graph) []string {
	// First-party packages that declare at least one function (any receiver/closure/
	// wrapper). A first-party package ABSENT from this set is types/consts only. The
	// synthesized package initializer is EXCLUDED — every package, types-only ones
	// included, gets an `init`, so counting it would mark nothing function-less — the
	// same init exclusion firstPartyScope applies so "function" means the same thing
	// in both places (CLAUDE.md: one notion of a real service function).
	hasFunc := map[string]bool{}
	for fn := range ssautil.AllFunctions(res.Program.Prog) {
		if fn.Pkg == nil || fn.Pkg.Pkg == nil || features.IsPackageInit(fn) {
			continue
		}
		if path := fn.Pkg.Pkg.Path(); res.Program.IsFirstPartyPath(path) {
			hasFunc[path] = true
		}
	}
	// The rendered components: packages that contributed a node to THIS graph (the one
	// "is this package visible in the rollup" predicate, shared with the rollup's own
	// node-count keys). The disclosure is anchored to them — a package no component
	// imports is genuinely unreferenced, not invisibly-imported, so it is never listed.
	component := g.nodePackageSet()
	omitted := map[string]bool{}
	for _, p := range res.Program.ServicePkgs {
		if !component[p.Pkg.Path()] {
			continue // only disclose under a package a reader can see (a rendered component)
		}
		for _, imp := range p.Pkg.Imports() {
			ip := imp.Path()
			if !res.Program.IsFirstPartyPath(ip) || hasFunc[ip] {
				// A stdlib/third-party import is not a missing INTERNAL package; a
				// first-party import that declares functions is either a component or
				// the (separate) unreachable-code concern — neither belongs here.
				continue
			}
			omitted[ip] = true
		}
	}
	return sortedKeys(omitted)
}

// rootByName returns the root function registered under name (a route or topic).
func rootByName(res *analyze.Result, name string) *ssa.Function {
	for _, r := range res.Roots.Roots {
		if r.Name == name {
			return r.Func
		}
	}
	return nil
}

// reachableFirstParty returns the first-party functions reachable from root,
// pruning the traversal at the boundary (it does not descend into dependencies).
func reachableFirstParty(res *analyze.Result, root *ssa.Function) map[*ssa.Function]bool {
	seen := make(map[*ssa.Function]bool)
	start := res.Graph.Node(root)
	if start == nil {
		return seen
	}
	visited := map[*cg.Node]bool{start: true}
	queue := []*cg.Node{start}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		seen[n.Func] = true
		for _, e := range n.Out {
			c := e.Callee
			if visited[c] || !res.Program.IsFirstPartyFunc(c.Func) {
				continue
			}
			visited[c] = true
			queue = append(queue, c)
		}
	}
	return seen
}

// fallible reports whether fn returns or propagates an error.
func fallible(fn *ssa.Function) bool { return features.Fallible(fn) }
