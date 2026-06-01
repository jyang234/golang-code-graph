// Package blindspots detects where the static analysis is blind at the service
// boundary, so a reviewer never operates on false completeness (static-extractor
// spec §7). The boundary subset of this manifest is part of the GATED artifact:
// if a PR introduces a dynamically-named publish or an unresolved dispatch at the
// boundary, the manifest changes and routes to a human — the one genuine hole
// becomes a tracked fact instead of a silent miss.
//
// The headline category is NonConstantBoundaryArg: a publish or outbound call
// whose target (event name, or peer/method/route) is not a string constant, so
// the effect is real but cannot be named.
package blindspots

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/features"
)

// Kind is a blind-spot category.
type Kind string

const (
	// NonConstantBoundaryArg is a publish/RPC with a non-literal target — the
	// gated, reviewable hole.
	NonConstantBoundaryArg Kind = "NonConstantBoundaryArg"
	// UnresolvedDispatch is a registration whose handler could not be resolved.
	UnresolvedDispatch Kind = "UnresolvedDispatch"
	// Reflect is reflective code, invisible to the call graph.
	Reflect Kind = "reflect"
	// HighFanOut is a dynamic-dispatch site the algorithm resolved to many
	// candidate callees — likely over-approximation (static-extractor §7).
	HighFanOut Kind = "HighFanOut"
	// Unsafe is a package using unsafe pointer tricks that can hide edges.
	Unsafe Kind = "unsafe"
	// Cgo is a package calling across the C boundary, invisible to the graph.
	Cgo Kind = "cgo"
	// Linkname is a package using //go:linkname, which links symbols outside the
	// visible call graph.
	Linkname Kind = "go:linkname"
)

// highFanOutThreshold is the callee count above which a single dynamic-dispatch
// site is flagged as likely over-approximation. The fixture's two-implementation
// interface (2 callees) stays well under it; a genuinely polymorphic site (a
// widely-implemented interface) trips it.
const highFanOutThreshold = 8

// Boundary reports whether a blind spot belongs to the GATED boundary subset.
// Only the categories that describe an inter-service boundary surface gate: a
// dynamically-named boundary effect and an unresolved entry-point registration.
// The graph-completeness disclosures (reflect, fan-out, unsafe/cgo/linkname) are
// keyed by first-party symbol and would churn the contract under internal
// refactoring, so they ride the non-gated graph view instead (static-extractor
// §7: "the boundary subset of this manifest is part of the gated artifact").
func (k Kind) Boundary() bool {
	return k == NonConstantBoundaryArg || k == UnresolvedDispatch
}

// BlindSpot is one disclosed gap. Fields are JSON-tagged for the gated artifact.
type BlindSpot struct {
	Kind   Kind   `json:"kind"`
	Site   string `json:"site"`
	Detail string `json:"detail"`
}

// Boundary returns the gated boundary subset of a manifest.
func Boundary(bs []BlindSpot) []BlindSpot { return filter(bs, Kind.Boundary) }

// Graph returns the non-gated graph-completeness subset of a manifest.
func Graph(bs []BlindSpot) []BlindSpot {
	return filter(bs, func(k Kind) bool { return !k.Boundary() })
}

func filter(bs []BlindSpot, keep func(Kind) bool) []BlindSpot {
	out := make([]BlindSpot, 0, len(bs))
	for _, b := range bs {
		if keep(b.Kind) {
			out = append(out, b)
		}
	}
	return out
}

// Detect returns every blind spot reachable in the analyzed program — both the
// gated boundary subset and the non-gated graph-completeness disclosures —
// sorted and de-duplicated for deterministic output. Callers split it with
// Boundary / Graph.
func Detect(res *analyze.Result, hints *features.HintSet) []BlindSpot {
	var out []BlindSpot

	// Unresolved handler registrations, surfaced by root discovery.
	for _, bs := range res.Roots.BlindSpots {
		out = append(out, BlindSpot{
			Kind:   UnresolvedDispatch,
			Site:   bs.Registrar,
			Detail: bs.Detail,
		})
	}

	// Boundary calls with non-constant targets, reflect, and high-fan-out dynamic
	// dispatch, in reachable first-party code.
	for _, n := range res.Graph.Nodes {
		fn := n.Func
		if !res.Program.IsFirstParty(fn.Pkg) {
			continue
		}
		site := fn.RelString(nil)
		perSite := map[ssa.CallInstruction]map[string]bool{}
		for _, e := range n.Out {
			callee := e.Callee.Func
			switch {
			case hints.IsPublish(callee):
				if !constStrings(e.Site, 1) {
					out = append(out, BlindSpot{
						Kind:   NonConstantBoundaryArg,
						Site:   site,
						Detail: "publish event name is not a string constant; the published event cannot be named statically",
					})
				}
			case hints.IsHTTP(callee):
				if !constStrings(e.Site, 3) {
					out = append(out, BlindSpot{
						Kind:   NonConstantBoundaryArg,
						Site:   site,
						Detail: "outbound peer/method/route is not a string constant; the dependency cannot be named statically",
					})
				}
			case pkgPathOf(callee) == "reflect":
				out = append(out, BlindSpot{
					Kind:   Reflect,
					Site:   site,
					Detail: "reflective call; downstream edges are invisible to the static call graph",
				})
			}
			if e.Site != nil {
				m := perSite[e.Site]
				if m == nil {
					m = map[string]bool{}
					perSite[e.Site] = m
				}
				m[callee.RelString(nil)] = true
			}
		}
		for _, callees := range perSite {
			if len(callees) > highFanOutThreshold {
				out = append(out, BlindSpot{
					Kind:   HighFanOut,
					Site:   site,
					Detail: fmt.Sprintf("a dynamic-dispatch site resolves to %d candidate callees; the call graph may be over-approximated here", len(callees)),
				})
			}
		}
	}

	// Package-level disclosures: unsafe, cgo, and go:linkname hide edges from the
	// call graph entirely.
	out = append(out, packageDisclosures(res)...)

	return dedupSort(out)
}

// packageDisclosures flags first-party packages that use unsafe, cgo, or a
// linkname directive — each of which can route control flow around the call
// graph.
func packageDisclosures(res *analyze.Result) []BlindSpot {
	if res.Service == nil {
		return nil
	}
	var out []BlindSpot
	for _, p := range res.Service.Packages {
		if !res.Program.IsFirstPartyPath(p.PkgPath) {
			continue
		}
		if p.Imports != nil && p.Imports["unsafe"] != nil {
			out = append(out, BlindSpot{Kind: Unsafe, Site: p.PkgPath,
				Detail: "package imports unsafe; pointer conversions can hide edges from the call graph"})
		}
		if usesCgo(p) {
			out = append(out, BlindSpot{Kind: Cgo, Site: p.PkgPath,
				Detail: "package uses cgo; calls across the C boundary are invisible to the call graph"})
		}
		if usesLinkname(p) {
			out = append(out, BlindSpot{Kind: Linkname, Site: p.PkgPath,
				Detail: "package uses //go:linkname; linked symbols bypass the visible call graph"})
		}
	}
	return out
}

// usesCgo reports whether any of a package's source files import "C" — the
// marker of a cgo package, whose calls across the C boundary are invisible to
// the call graph.
func usesCgo(p *packages.Package) bool {
	for _, f := range p.Syntax {
		for _, imp := range f.Imports {
			if imp.Path != nil && imp.Path.Value == `"C"` {
				return true
			}
		}
	}
	return false
}

// usesLinkname reports whether any of a package's source files carry a
// //go:linkname directive.
func usesLinkname(p *packages.Package) bool {
	for _, f := range p.Syntax {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(c.Text, "//go:linkname") {
					return true
				}
			}
		}
	}
	return false
}

// constStrings reports whether the call site supplies at least n string arguments
// and the first n are all constants — the condition for naming the boundary
// effect.
func constStrings(site ssa.CallInstruction, n int) bool {
	args := features.StringArgs(site)
	if len(args) < n {
		return false
	}
	for i := 0; i < n; i++ {
		if _, ok := features.ConstString(args[i]); !ok {
			return false
		}
	}
	return true
}

func pkgPathOf(fn *ssa.Function) string {
	if fn == nil || fn.Pkg == nil || fn.Pkg.Pkg == nil {
		return ""
	}
	return fn.Pkg.Pkg.Path()
}

func dedupSort(in []BlindSpot) []BlindSpot {
	seen := make(map[BlindSpot]bool, len(in))
	out := make([]BlindSpot, 0, len(in))
	for _, b := range in {
		if seen[b] {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		return a.Detail < b.Detail
	})
	return out
}
