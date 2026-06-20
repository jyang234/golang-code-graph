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

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/ssabuild"
)

// Kind is a blind-spot category.
type Kind string

const (
	// NonConstantBoundaryArg is a publish/RPC with a non-literal target — the
	// gated, reviewable hole.
	NonConstantBoundaryArg Kind = "NonConstantBoundaryArg"
	// UnresolvedDispatch is a registration whose handler could not be resolved.
	UnresolvedDispatch Kind = "UnresolvedDispatch"
	// UnresolvedCall is a func-value call site the algorithm resolved to NO
	// callee — a func value whose target lies outside the visible address-taken
	// set (an external callback, a field set elsewhere, a registry populated past
	// the rooted entry points). Unlike UnresolvedDispatch (a boundary
	// registration) this is an in-body, graph-completeness gap: the invoked
	// function and all its downstream edges and effects are absent from the
	// graph, so a must_not_reach over the site must abstain, not prove absence.
	UnresolvedCall Kind = "UnresolvedCall"
	// ConcurrentDispatch is the goroutine sibling of UnresolvedCall: a func-value
	// call the algorithm resolved to NO callee whose dispatching instruction is a
	// `go` statement (*ssa.Go). The concurrency is a VERIFIED fact read straight
	// from the SSA — not an inference — so the machine states "an asynchronous body
	// is hidden here" instead of the generic "a call is hidden". It is the same
	// graph-completeness gap as UnresolvedCall (the goroutine's body and downstream
	// edges are absent) and Bin A — irreducible to static, never reclaimable by
	// --reclaim — so a must_not_reach over the site must abstain. Split out so a
	// reviewer reads the recovered shape rather than a flattened catch-all; a
	// resolved `go` dispatch is NOT here — its body is in the graph and its
	// concurrency rides the edge's Concurrent flag.
	ConcurrentDispatch Kind = "ConcurrentDispatch"
	// ExternalBoundaryCall is a call from first-party code into a THIRD-PARTY
	// (non-stdlib) package that is not already a classified boundary effect
	// (HTTP/DB/bus/telemetry). The callee is KNOWN — its package is named — but its
	// body lies outside the analyzed module, so its downstream edges and effects are
	// invisible (the cgate CustomerIO-SDK seam: the outbound HTTPS send happens
	// inside the vendored client). It is the machine-verified "control leaves into
	// package X here" disclosure, the unclassified-external-dependency surface a
	// reviewer either classifies, annotates, or accepts as out of scope. Stdlib is
	// excluded (the language platform, not a dependency boundary); classified
	// boundaries are excluded (already disclosed as typed effects); and infrastructure
	// plumbing is excluded via the exempt list (HintSet.IsExternalBoundaryExempt:
	// OpenTelemetry built-in, plus any config static.externalBoundaryExempt prefixes).
	//
	// Unlike UnresolvedCall/ConcurrentDispatch (UNKNOWN target — could dispatch back
	// into first-party code and reach a forbidden sink, so reach must abstain) this
	// has a known external target that is the SAME leaf the reachability index
	// already stops at (graph.Index drops external edges). So it is DISCLOSURE-ONLY:
	// reach.frontierBlindSiteWith deliberately skips it, leaving every must_not_reach
	// verdict unchanged — it discloses the accepted external-leaf boundary, it does
	// not redefine it.
	ExternalBoundaryCall Kind = "ExternalBoundaryCall"
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
	// ImpeachmentSeam is a human-RATIFIED seam: a site where behavior proved the
	// static over-approximation's disclosure incomplete (the behavioral-impeachment
	// loop's blind-spot repair, plan §8). Unlike the others it is not auto-detected
	// from code — it is DECLARED in config (static.declaredBlindSpots), CODEOWNER-
	// gated, each carrying the impeachment witness as its reason. flowmap merges it
	// so the next run abstains at the seam (NEVER → CANT-PROVE) — the safe direction.
	ImpeachmentSeam Kind = "ImpeachmentSeam"
)

// Kinds returns every blind-spot category. It exists so exhaustiveness guards —
// e.g. a classifier that must assign each kind a bin — can iterate the full set and
// fail when a new kind is added without handling. Keep it in sync with the consts
// above (a new const belongs here).
func Kinds() []Kind {
	return []Kind{
		NonConstantBoundaryArg, UnresolvedDispatch, UnresolvedCall, ConcurrentDispatch, ExternalBoundaryCall, Reflect, HighFanOut, Unsafe, Cgo, Linkname, ImpeachmentSeam,
	}
}

// Recognized reports whether k names a known blind-spot category. It backs
// validation of config-declared seams (graphio.mergeDeclaredBlindSpots): a seam
// declared with a Kind outside this set is a config error, not a silent passthrough
// of an unknown category. Derived from Kinds() so a new const is covered without a
// second edit.
func Recognized(k Kind) bool {
	for _, known := range Kinds() {
		if k == known {
			return true
		}
	}
	return false
}

// declaredKinds are the categories that originate ONLY from human config
// declaration (static.declaredBlindSpots), NEVER from Detect. It is the single
// source for the detected-vs-declared partition: Ratified() derives from it, and
// TestDetectEmitsNoDeclaredKind asserts Detect's output stays disjoint from it, so
// the load-bearing assumption "a Ratified kind in a graph came from CODEOWNER-gated
// config, not auto-detection" is enforced rather than merely asserted in prose. A new
// declared category is added here once and both derivations pick it up.
func declaredKinds() []Kind {
	return []Kind{ImpeachmentSeam}
}

// Ratified reports whether a blind spot was DECLARED by a human (config-ratified),
// rather than auto-detected from code — membership in the declaredKinds set
// (currently just ImpeachmentSeam, the behavioral-impeachment enactment in
// static.declaredBlindSpots). It is the single predicate every "this is a reviewed
// declaration, not undisclosed drift" branch consults — the blind-spot ratchet
// (review.newBlindSpots) and the frontier reclaim markers (frontier.Classify) both
// exclude a ratified kind, so a ratified seam neither re-blocks the change that
// ratified it nor churns the reclaim ratios. Centralizing it here (paralleling
// Boundary), derived from the one declaredKinds source, means a future declared kind
// is covered in ONE edit instead of two hand-kept literal compares with divergent
// spellings; the producer boundary (Detect never emits a declared kind) is guarded by
// TestDetectEmitsNoDeclaredKind.
func (k Kind) Ratified() bool {
	for _, d := range declaredKinds() {
		if k == d {
			return true
		}
	}
	return false
}

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

// IsDisclosureOnlyFrontier reports whether a blind spot of this kind discloses a
// KNOWN out-of-module leaf the analysis already stops at — so it must NOT act as a
// reachability or severance frontier. Such a kind neither blinds a must_not_reach
// proof (fitness.firstReachBlinding skips it) nor enters the frontier marker set /
// ReclaimableShare (frontier.Classify skips it): the effect it names is the same
// leaf the call graph already terminates at, hiding no in-scope path and severing
// nothing. ExternalBoundaryCall is the one such kind today. Centralized here so the
// two skip sites read one predicate instead of each re-deciding by literal Kind —
// a future disclosure-only kind flips this and both honor it.
func (k Kind) IsDisclosureOnlyFrontier() bool {
	return k == ExternalBoundaryCall
}

// Severity is the signal/noise TIER an ExternalBoundaryCall carries. It exists so a
// bare blind-spot count (dominated ~85% by framework/utility handoffs) is readable:
// a reviewer separates the effect-bearing seams from the pure-compute plumbing
// without re-deriving it (§21.A). It is DISCLOSURE-ONLY — no verdict, ratchet, or
// reachability computation reads it; it (mis)prioritizes attention, never gates, and
// a trivial-tagged spot is still detected and counted.
type Severity string

const (
	// SeverityEffectBearing tags an ExternalBoundaryCall into a dependency whose
	// handoff can carry a real external effect (a cloud-SDK send, a DB driver) — the
	// signal a reviewer acts on. It is the DEFAULT for an EBC: a package is treated
	// as effect-bearing unless explicitly known-benign, so an unrecognized dependency
	// stays in the signal tier rather than being silently demoted (fail toward
	// disclosure).
	SeverityEffectBearing Severity = "effect-bearing"
	// SeverityTrivial tags an ExternalBoundaryCall into pure-compute / framework
	// plumbing (uuid generation, an HTTP router's helpers, codegen runtime) — disclosed
	// for completeness but not the effect surface. Driven by the known-benign set
	// (HintSet.IsExternalBoundaryTrivial: a built-in default plus any
	// static.externalBoundaryTrivial prefixes).
	SeverityTrivial Severity = "trivial"
)

// externalPackagePrefix is the literal Detail prefix Detect writes before an
// ExternalBoundaryCall's target package; ExternalPackage parses it back. Kept beside
// the writer so the two cannot drift (CLAUDE.md: one source of truth).
const externalPackagePrefix = "hands off to external package "

// ExternalPackage returns the third-party package path an ExternalBoundaryCall's
// Detail names, or "" when detail is not in that form. Render-only: it recovers for
// display the package Detect embedded in the prose, so a boundary disclosure node can
// be labeled with its dependency (§21.B). The structural tier (BlindSpot.Severity) is
// the reliable distinguisher; this is the human-readable enrichment beside it, so a
// parse miss degrades to the tier alone rather than a silent collision.
func ExternalPackage(detail string) string {
	rest, ok := strings.CutPrefix(detail, externalPackagePrefix)
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// BlindSpot is one disclosed gap. Fields are JSON-tagged for the gated artifact.
type BlindSpot struct {
	Kind   Kind   `json:"kind"`
	Site   string `json:"site"`
	Detail string `json:"detail"`
	// Severity is the signal/noise tier, set ONLY for ExternalBoundaryCall (empty for
	// every other kind, and for an EBC from a graph built before the tier existed).
	// Disclosure-only — see the Severity type. It is a pure function of the target
	// package (which Detail names), so it never adds an independent ordering dimension
	// to SortBlindSpots, and two spots equal on (Kind, Site, Detail) are equal on it.
	Severity Severity `json:"severity,omitempty"`
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
	cfg := res.Config
	if cfg == nil {
		cfg = &config.Config{}
	}
	fanOut := cfg.FanOutThreshold()

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
		// init is an RTA seed (it recovers registration addresses), not rendered
		// service behavior — graphio excludes it from the graph, so its disclosures
		// (reflect/fan-out/etc.) must be excluded too, or the manifest would gain a
		// blind spot for a node that is not in the graph it annotates.
		if features.IsPackageInit(fn) {
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
			case features.PkgPath(callee) == "reflect":
				out = append(out, BlindSpot{
					Kind:   Reflect,
					Site:   site,
					Detail: "reflective call; downstream edges are invisible to the static call graph",
				})
			case isExternalBoundary(res.Program, hints, callee):
				// A handoff into a third-party package we do not analyze and have not
				// classified as a typed boundary effect. Detail names the package, not
				// the symbol, so multiple callees in one package at this site dedup to a
				// single per-(site, package) disclosure. The signal/noise tier rides
				// along: a known-benign (pure-compute/framework) target is trivial, every
				// other dependency effect-bearing (the default — disclose, don't pre-judge).
				sev := SeverityEffectBearing
				if hints.IsExternalBoundaryTrivial(callee) {
					sev = SeverityTrivial
				}
				out = append(out, BlindSpot{
					Kind:     ExternalBoundaryCall,
					Site:     site,
					Detail:   externalPackagePrefix + features.PkgPath(callee) + "; its behavior is outside the analyzed module and invisible to the static call graph",
					Severity: sev,
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
			if len(callees) > fanOut {
				out = append(out, BlindSpot{
					Kind:   HighFanOut,
					Site:   site,
					Detail: fmt.Sprintf("a dynamic-dispatch site resolves to %d candidate callees; the call graph may be over-approximated here", len(callees)),
				})
			}
		}

		// Zero-resolution is the mirror of HighFanOut and is INVISIBLE to the edge
		// loop above: a call site the algorithm resolved to no callee produces no
		// out-edge, so it can only be found by walking fn's call instructions and
		// subtracting the sites that DID resolve (perSite). A func-value call always
		// invokes a real function at runtime, so a site we cannot bind to any target
		// is a genuine hole — its callee's edges and effects are absent from the
		// graph. Disclosed so must_not_reach abstains (noPathFound) instead of
		// laundering the gap into a provenAbsent.
		out = append(out, unresolvedFuncValueCalls(fn, site, perSite)...)
	}

	// Package-level disclosures: unsafe, cgo, and go:linkname hide edges from the
	// call graph entirely.
	out = append(out, packageDisclosures(res)...)

	return dedupSort(out)
}

// unresolvedFuncValueCalls flags every func-value call in fn that the algorithm
// resolved to NO callee — the zero-resolution mirror of HighFanOut. resolved is
// the per-site callee map the edge loop already built from fn's out-edges; a call
// instruction absent from it produced no edge.
//
// Scope is deliberately func-value calls only. A direct (static) call is an
// ordinary edge, not a gap. An interface invoke is excluded because RTA resolves
// invokes soundly against the reachable concrete-type set, so a zero-resolution
// invoke is a genuinely dead type rather than a hidden edge; only a func value
// can carry a callee from OUTSIDE the visible address-taken set (an external
// callback, a struct field assigned elsewhere, a registry populated past the
// rooted entry points), which is the seam that vanishes silently. Builtins are
// excluded — they are not call-graph edges.
//
// Each gap is reported with the shape the SSA proves rather than one flattened
// kind: a `go` dispatch (*ssa.Go, read via the shared features.IsConcurrentSite)
// is a ConcurrentDispatch — the machine states the hidden body is asynchronous —
// and every other zero-resolution func value is an UnresolvedCall. Both carry the
// func value's signature when known, so even the irreducible residue names its
// type instead of "unknown". This only re-labels sites already flagged; it adds
// and removes none, so reachability and every verdict are unchanged.
func unresolvedFuncValueCalls(fn *ssa.Function, site string, resolved map[ssa.CallInstruction]map[string]bool) []BlindSpot {
	var out []BlindSpot
	for _, b := range fn.Blocks {
		for _, instr := range b.Instrs {
			call, ok := instr.(ssa.CallInstruction)
			if !ok {
				continue
			}
			cc := call.Common()
			if cc.IsInvoke() || cc.StaticCallee() != nil {
				continue
			}
			if _, isBuiltin := cc.Value.(*ssa.Builtin); isBuiltin {
				continue
			}
			if len(resolved[call]) != 0 {
				continue
			}
			sig := ""
			if s := cc.Signature(); s != nil {
				sig = " of type " + s.String()
			}
			if features.IsConcurrentSite(call) {
				out = append(out, BlindSpot{
					Kind:   ConcurrentDispatch,
					Site:   site,
					Detail: "a goroutine (`go`) dispatches a func value" + sig + " resolved to no callee; the concurrent body and its downstream edges are invisible to the static call graph",
				})
				continue
			}
			out = append(out, BlindSpot{
				Kind:   UnresolvedCall,
				Site:   site,
				Detail: "a func-value call" + sig + " resolved to no callee; the invoked function and its downstream edges are invisible to the static call graph",
			})
		}
	}
	return out
}

// isExternalBoundary reports whether a call to callee is an ExternalBoundaryCall:
// a handoff into a third-party (non-stdlib) package whose behavior the analysis
// does not see and that is not already a classified boundary effect. First-party
// callees are ordinary in-graph edges; stdlib is the language platform, not a
// dependency boundary (and reach already leafs it); a callee the hints classify as
// telemetry/publish/consume/DB/HTTP is disclosed as a typed boundary effect, so
// re-flagging it here would double-count. What remains — an unclassified
// third-party dependency — is the surface this discloses.
func isExternalBoundary(prog *ssabuild.Program, hints *features.HintSet, callee *ssa.Function) bool {
	path := features.PkgPath(callee)
	if path == "" || prog.IsFirstPartyPath(path) || features.IsStdlib(path) {
		return false
	}
	if hints.IsExternalBoundaryExempt(callee) {
		// Observability/infrastructure plumbing (OTel built-in, plus any
		// static.externalBoundaryExempt prefixes) — a known dependency, not a
		// boundary worth disclosing per the noise it would otherwise generate.
		return false
	}
	return !hints.IsTelemetry(callee) && !hints.IsPublish(callee) && !hints.IsConsume(callee) &&
		!hints.IsDB(callee) && !hints.IsHTTP(callee)
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
	SortBlindSpots(out)
	return out
}

// SortBlindSpots sorts a manifest in place by the canonical, run-independent order
// (Kind, then Site, then Detail) — the ONE comparator every blind-spot ordering
// uses, so a manifest's byte form does not depend on detection or declaration
// order. Both Detect (via dedupSort) and the graphio declared-seam merge sort
// through here; keeping the tie-break in one place is what stops the two copies
// from drifting (CLAUDE.md "one source of truth").
func SortBlindSpots(bs []BlindSpot) {
	sort.Slice(bs, func(i, j int) bool {
		a, b := bs[i], bs[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		return a.Detail < b.Detail
	})
}
