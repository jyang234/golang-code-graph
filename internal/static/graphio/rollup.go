package graphio

// Package rollup view: the C3 (component) altitude above the C4 (function) call
// graph. It groups first-party nodes by their defining package (a COMPONENT),
// collapses the function-level edges to component→component dependencies, and
// overlays the external-system effects — both the statically RESOLVED ones (typed
// boundary edges) and the DISCLOSED-but-blind ones (ExternalBoundaryCall handoffs the
// static graph cannot see past). It is a pure, fully-sorted post-process of the
// already-canonical Graph — deterministic by construction, and disclosure-only: it
// reads nothing the graph did not already emit and computes no verdict. The whole
// point is altitude — at C3 an architecture-violating change is one visible edge
// rather than rename noise.
//
// Edge provenance is split into two classes, never conflated (this is what keeps a
// component diff honest):
//   - CODE edges (Kind "call"/"effect") — a statically resolved call or boundary
//     effect; a delta here is a real dependency change.
//   - DISCLOSURE edges (Kind "disclosed") — a dashed effect the graph DISCLOSES but
//     cannot resolve (an ExternalBoundaryCall behind a seam); a delta here is only a
//     newly-DOCUMENTED effect, not new architecture.

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// Edge-kind values. CODE = {call, effect}; WIRING = {wiring}; DISCLOSURE = {disclosed}.
// The class an edge belongs to is its diff split, so the two are named once here.
const (
	// RollupCall is a component→component dependency: a resolved first-party call
	// crossing a package boundary. Solid.
	RollupCall = "call"
	// RollupWiring is a dependency-injection BACK-EDGE into a composition root: a
	// first-party component invokes a func value (a closure or method value) whose
	// body is DEFINED in a `package main` command and that main INJECTED into the
	// component. It is a real resolved call — the body genuinely lives in main — but
	// the dependency reads backwards: main wired the value INTO the component; the
	// component does not depend on main. A composition-root package CANNOT be imported,
	// so EVERY edge whose target is a composition root is necessarily such an injected
	// func value (there is no other way for first-party code to reach a function
	// defined in `package main`) — which is what makes the reclassification sound. That
	// soundness rests on identifying the composition root AUTHORITATIVELY: it is the
	// SSA main-package set the graph carries in CompositionRoots (roots.KindMain), not
	// an FQN guess, so a non-main package that merely declares a `func main` is never
	// treated as a root. (compositionRootSet falls back to an FQN heuristic only for a
	// legacy graph that predates the field, where that one residual ambiguity is
	// bounded and documented.) Carried apart from RollupCall so the C3 view is not
	// inverted by assembly wiring and the rollup diff does not file an injected-closure
	// swap under the code bin (see PackageRollupDiff). CODE-adjacent and solid-by-
	// default, but its own class: External() is false (the target is a first-party
	// component) yet it is not a domain call.
	RollupWiring = "wiring"
	// RollupEffect is a component→external-system effect: a resolved typed boundary
	// edge (a DB op, a bus publish/consume, an outbound peer call). Solid.
	RollupEffect = "effect"
	// RollupDisclosed is a component→external-system effect the static graph DISCLOSES
	// but cannot resolve: an effect-bearing ExternalBoundaryCall (a handoff into a
	// third-party dependency, e.g. a Customer.io send behind a func() seam). Dashed —
	// it is documented, not statically proven, so a consumer never reads it as a
	// resolved dependency.
	RollupDisclosed = "disclosed"
)

// RollupRoot is the Component.Role marker for a COMPOSITION-ROOT component: the
// package declaring `func main` (the `cmd/<svc>` assembly point). It is where the
// program is wired together, so nothing should read as depending on it — a consumer
// folds every edge incident to it (both the cmd→X constructor wiring and the X→cmd
// injected-closure back-edges) to recover the domain topology without assembly noise.
// A pure function of the graph (the main package is unambiguous) and disclosure-only.
const RollupRoot = "composition_root"

// PackageRollup is the component-level view of a Graph.
type PackageRollup struct {
	Components []Component  `json:"components"`
	Edges      []RollupEdge `json:"edges"`
}

// Component is one first-party Go package and how many graph nodes rolled up into it.
type Component struct {
	// Package is the full import path — the component's stable identity (an edge's
	// From/To reference it). Name is its last path segment, for display.
	Package string `json:"package"`
	Name    string `json:"name"`
	Nodes   int    `json:"nodes"`
	// Role marks a component whose place in the architecture a consumer must read
	// specially. The only value today is RollupRoot ("composition_root"): the
	// `package main` assembly point. Empty (omitted) for an ordinary domain
	// component. Disclosure-only — it names a role, it computes no verdict; an old
	// consumer that ignores it sees today's behavior.
	Role string `json:"role,omitempty"`
	// Band is the component's ARCHITECTURAL band for the C3 view — the SEMANTIC role
	// read off the import path (transport / application / provisioning / storage /
	// infrastructure / tests), the missing grouping key that lets a render lane the
	// component boxes by role. Populated by classifyBand for every NON-root component;
	// the composition root is left bandless (it is named by Role, a graph fact, so a
	// grouped render draws it outside the bands) and an external system stays unbanded
	// (a boundary, not a first-party component). NOT the topological layering layer:
	// bands are a semantic name-read axis, layers a call-rank the layering policy gates
	// on — see band.go. A VIEW, NEVER A GATE: it carries no verdict and nothing should
	// block on it; an old consumer that ignores it sees today's behavior.
	Band string `json:"band,omitempty"`
}

// RollupEdge is one collapsed component-level edge. From is always a component
// (package import path). To is a component (Kind "call") or an external-system id
// (Kind "effect"/"disclosed"): a boundary peer token ("db", "bus", "credit-bureau")
// for a resolved effect, or the third-party import path for a disclosed handoff.
type RollupEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	// Note carries the human Annotation context (note/claim) attached to a disclosed
	// edge's seam, when one exists — the "what this blind effect actually does" the
	// machine cannot prove. Empty for resolved edges and for unannotated disclosures.
	Note string `json:"note,omitempty"`
}

// External reports whether the edge's To is an external system rather than a
// first-party component — true for an effect/disclosed edge, false for a first-party
// call OR a composition-root wiring edge (both target a component box). Spelled as a
// positive enumeration, not "everything but a call", so a future kind cannot silently
// fall into the external bin.
func (e RollupEdge) External() bool { return e.Kind == RollupEffect || e.Kind == RollupDisclosed }

// Resolved reports whether the edge is statically proven (solid) rather than a
// disclosed-but-blind effect (dashed). True for call/wiring/effect, false for
// disclosed. The diff's code-vs-disclosure split keys on it (wiring is split out
// ahead of this — see PackageRollupDiff.add).
func (e RollupEdge) Resolved() bool { return e.Kind != RollupDisclosed }

// Wiring reports whether the edge is a dependency-injection back-edge into a
// composition root (RollupWiring) — classified apart from a domain call so it stays
// out of the diff's code bin and a consumer can fold it.
func (e RollupEdge) Wiring() bool { return e.Kind == RollupWiring }

// compositionRootSet is the set of composition-root packages (the `package main`
// commands) for the rollup. It PREFERS the graph's authoritative CompositionRoots
// — the SSA main-package set recorded at build (graphio.compositionRoots, from
// roots.KindMain), exact and not an FQN guess. It falls back to recognizing a
// package-level `func main` by FQN ONLY for a legacy graph built before that field
// existed (e.g. an old base passed to --diff), so such a graph still classifies
// wiring instead of reading every injected back-edge as a code dependency and
// churning the diff. The fallback is a compatibility shim, not a second source of
// truth: it is deliberately stricter than groundwork/fitness.proposeLayers's
// `strings.HasSuffix(fqn, ".main")` — it requires FQN == Package+".main", so a
// method named main ("(*pkg.T).main") is excluded — and it carries the residual
// ambiguity that a non-main package declaring a package-level `func main` is
// mistaken for a root. A graph built by this flowmap never reaches the fallback, so
// that ambiguity cannot arise on a current graph; it is bounded to legacy input.
func compositionRootSet(g *Graph) map[string]bool {
	roots := make(map[string]bool, len(g.CompositionRoots))
	if len(g.CompositionRoots) > 0 {
		for _, pkg := range g.CompositionRoots {
			roots[pkg] = true
		}
		return roots
	}
	for _, n := range g.Nodes {
		if n.Package != "" && n.FQN == n.Package+".main" {
			roots[n.Package] = true
		}
	}
	return roots
}

// RollupByPackage computes the component view. Pure function of g; every list is
// sorted on intrinsic keys so the result is byte-identical across runs.
func (g *Graph) RollupByPackage() *PackageRollup {
	// First-party node → its package, plus per-package node counts. A synthetic node
	// (empty Package) is not a component and anchors no edge.
	pkgOf := make(map[string]string, len(g.Nodes))
	counts := map[string]int{}
	for _, n := range g.Nodes {
		if n.Package == "" {
			continue
		}
		pkgOf[n.FQN] = n.Package
		counts[n.Package]++
	}
	roots := compositionRootSet(g)

	components := make([]Component, 0, len(counts))
	for pkg, c := range counts {
		comp := Component{Package: pkg, Name: lastSegment(pkg), Nodes: c}
		if roots[pkg] {
			comp.Role = RollupRoot // the composition root is named by Role, not banded
		} else {
			comp.Band = classifyBand(pkg) // semantic C3 lane for every non-root component
		}
		components = append(components, comp)
	}
	sort.Slice(components, func(i, j int) bool { return components[i].Package < components[j].Package })

	type edgeKey struct{ from, to, kind string }
	seen := map[edgeKey]bool{}
	notes := map[edgeKey]map[string]bool{} // disclosed edges only: distinct annotation notes

	// CODE edges: component→component calls and component→external resolved effects.
	for _, e := range g.Edges {
		from := pkgOf[e.From]
		if from == "" {
			continue // a synthetic/out-of-graph caller anchors no component
		}
		if peer := boundaryPeer(e.To); peer != "" {
			seen[edgeKey{from, peer, RollupEffect}] = true
			continue
		}
		to := pkgOf[e.To]
		if to == "" || to == from {
			continue // out-of-graph target, or an intra-package call (not a component edge)
		}
		// A back-edge INTO a composition root from a domain component is dependency-
		// injection WIRING, not a domain call: the target is a func value (closure or
		// method value) defined in `package main` and injected here, so the dependency
		// reads backwards. Sound to reclassify wholesale — main cannot be imported, so
		// there is no other kind of X→root edge to mistake for a real call. The cmd→X
		// constructor wiring (from is the root) deliberately stays a call; the RollupRoot
		// marker lets a consumer fold it too if it wants.
		kind := RollupCall
		if roots[to] && !roots[from] {
			kind = RollupWiring
		}
		seen[edgeKey{from, to, kind}] = true
	}

	// DISCLOSURE edges: effect-bearing ExternalBoundaryCall handoffs. A trivial EBC
	// (uuid/framework plumbing — Severity trivial) is NOT an effect, so it is excluded
	// to keep the component view signal; the Severity tier is the one source for that
	// benign/effect-bearing split. Each carries its human annotation note when one is
	// attached to the seam (keyed by site+kind, the same join the manifest uses).
	annNote := rollupAnnotationNotes(g)
	for _, b := range g.BlindSpots {
		if b.Kind != blindspots.ExternalBoundaryCall || b.Severity == blindspots.SeverityTrivial || b.Package == "" {
			continue
		}
		from := pkgOf[b.Site]
		if from == "" {
			continue
		}
		k := edgeKey{from, b.Package, RollupDisclosed}
		seen[k] = true
		if note := annNote[siteKind{b.Site, string(b.Kind)}]; note != "" {
			if notes[k] == nil {
				notes[k] = map[string]bool{}
			}
			notes[k][note] = true
		}
	}

	edges := make([]RollupEdge, 0, len(seen))
	for k := range seen {
		re := RollupEdge{From: k.from, To: k.to, Kind: k.kind}
		if ns := notes[k]; len(ns) > 0 {
			re.Note = joinSortedSet(ns, "; ")
		}
		edges = append(edges, re)
	}
	sort.Slice(edges, func(i, j int) bool { return rollupEdgeLess(edges[i], edges[j]) })

	return &PackageRollup{Components: components, Edges: edges}
}

// rollupEdgeLess is the total intrinsic order for component edges: From, then To, then
// Kind. Note is presentation, never identity, so it is not a sort dimension (two edges
// equal on (From, To, Kind) are the same edge — the dedup map already guarantees that).
func rollupEdgeLess(a, b RollupEdge) bool {
	if a.From != b.From {
		return a.From < b.From
	}
	if a.To != b.To {
		return a.To < b.To
	}
	return a.Kind < b.Kind
}

// siteKind keys an annotation by the (site, kind) pair it attaches to — the same key
// the manifest matches an annotation to its blind spot on.
type siteKind struct{ site, kind string }

// rollupAnnotationNotes indexes the human context per (site, kind): the Claim (the
// structured "what this effect does") when present, else the freeform Note. A
// disclosed component edge reads it to carry the reviewer's explanation of a blind
// effect the machine cannot prove. Graph annotations are already deduped per (Site, Kind)
// by mergeAnnotations (kept the lexically-smallest on a collision), so the last-write here
// is deterministic — it never sees two notes for one key on a graph produced by Build.
func rollupAnnotationNotes(g *Graph) map[siteKind]string {
	if len(g.Annotations) == 0 {
		return nil
	}
	out := make(map[siteKind]string, len(g.Annotations))
	for _, a := range g.Annotations {
		note := a.Claim
		if note == "" {
			note = a.Note
		}
		if note != "" {
			out[siteKind{a.Site, a.Kind}] = note
		}
	}
	return out
}

// boundaryPeer extracts the external-system id from a typed boundary edge target —
// the first whitespace-delimited token after the "boundary:" prefix ("boundary:db SELECT
// applicants" → "db", "boundary:credit-bureau GET /score/{id}" → "credit-bureau"). The
// peer is the C3 altitude for an external effect: every DB op collapses to "db", every
// publish to "bus", every call to a named peer to that peer. Returns "" for a non-boundary
// target (a first-party FQN), which is how the caller tells a call edge from an effect
// edge.
//
// The "boundary:<peer> <op>" shape is produced in labels.go and read field-wise the same
// way by the budget/contract/frontier consumers (strings.Fields after the prefix); this
// uses strings.Fields too so it cannot drift from that convention on odd whitespace.
func boundaryPeer(to string) string {
	rest, ok := strings.CutPrefix(to, "boundary:")
	if !ok {
		return ""
	}
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	return ""
}

// lastSegment is the final path segment of an import path — the package's bare name,
// the component's display label ("example.com/svc/internal/storage" → "storage").
func lastSegment(importPath string) string {
	if i := strings.LastIndexByte(importPath, '/'); i >= 0 {
		return importPath[i+1:]
	}
	return importPath
}

// joinSortedSet joins a set's members in lexical order (an intrinsic, run-independent
// tie-break, never map-iteration order) with sep — so an aggregated note is byte-stable.
func joinSortedSet(set map[string]bool, sep string) string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, sep)
}

// PackageRollupDiff is the component delta between two rollups, split THREE ways so a
// code change (a real new/dropped dependency or effect) is NEVER conflated with either
// a wiring change (which closure the composition root injects) or a disclosure change
// (a newly-documented or removed blind effect). Each list is sorted. Conflating the
// classes is the failure mode this split exists to prevent: pure instrumentation
// (annotating a seam) or pure re-wiring (swapping an injected handler) would otherwise
// read as an architecture change.
type PackageRollupDiff struct {
	CodeAdded   []RollupEdge `json:"code_added"`
	CodeRemoved []RollupEdge `json:"code_removed"`
	// WiringAdded/Removed are the composition-root DI back-edges (RollupWiring) that
	// changed. They are split out of Code* precisely so a change in WHICH closure main
	// injects (a different error-handler, a swapped factory) does not read as a new/
	// dropped domain dependency — the diff-pollution harm the wiring class exists to
	// fix. Classified, never dropped: the delta is still disclosed, just in its own bin.
	WiringAdded       []RollupEdge `json:"wiring_added"`
	WiringRemoved     []RollupEdge `json:"wiring_removed"`
	DisclosureAdded   []RollupEdge `json:"disclosure_added"`
	DisclosureRemoved []RollupEdge `json:"disclosure_removed"`
	// Caveats discloses a base↔branch SUBSTRATE skew (empty base, --algo/tool/reclaimer
	// mismatch, or a base/branch built before per-node package) that would paint a
	// non-code difference as a code/disclosure delta. Same honesty channel the
	// call-graph diff carries (provenanceCaveats); a skew silently shown as added/removed
	// edges is exactly the confidently-wrong delta the prime directive forbids. Omitted
	// when the two sides are comparable, so a clean diff stays caveat-free.
	Caveats []string `json:"caveats,omitempty"`
}

// RollupDiff computes the component delta between two GRAPHS (base → branch). It takes
// the graphs, not pre-built rollups, so it can disclose the base↔branch substrate skew
// (Caveats) the same way the call-graph MermaidDiff does — a rollup of two
// differently-built graphs would otherwise read precision/tool/reclaimer differences as
// architecture changes. An edge's identity is (From, To, Kind); Note is presentation, so
// a pure note change is not a delta. Symmetric by construction — swapping base and branch
// swaps every Added list with the matching Removed.
func RollupDiff(base, branch *Graph) *PackageRollupDiff {
	rb, rbr := base.RollupByPackage(), branch.RollupByPackage()
	d := diffRollups(rb, rbr)
	d.Caveats = rollupDiffCaveats(base, branch, rb, rbr)
	return d
}

// diffRollups is the pure code-vs-disclosure split between two already-built rollups —
// the diff logic without the graph-level provenance disclosure, so the split/symmetry
// invariant can be tested on rollups directly. Each output list is sorted because the
// inputs (rb.Edges / rbr.Edges) are sorted by RollupByPackage and the filtering append
// preserves that order; sortRollupEdges then makes the ordering contract hold regardless
// of how a caller built the rollups.
func diffRollups(base, branch *PackageRollup) *PackageRollupDiff {
	baseSet := rollupEdgeSet(base)
	branchSet := rollupEdgeSet(branch)
	d := &PackageRollupDiff{
		CodeAdded:         []RollupEdge{},
		CodeRemoved:       []RollupEdge{},
		WiringAdded:       []RollupEdge{},
		WiringRemoved:     []RollupEdge{},
		DisclosureAdded:   []RollupEdge{},
		DisclosureRemoved: []RollupEdge{},
	}
	for _, e := range branch.Edges {
		if !baseSet[rollupEdgeID(e)] {
			d.add(e, true)
		}
	}
	for _, e := range base.Edges {
		if !branchSet[rollupEdgeID(e)] {
			d.add(e, false)
		}
	}
	sortRollupEdges(d.CodeAdded)
	sortRollupEdges(d.CodeRemoved)
	sortRollupEdges(d.WiringAdded)
	sortRollupEdges(d.WiringRemoved)
	sortRollupEdges(d.DisclosureAdded)
	sortRollupEdges(d.DisclosureRemoved)
	return d
}

// rollupDiffCaveats is the base↔branch skew disclosure for a rollup diff: the call-graph
// provenance caveats (empty base, --algo/tool/reclaimer mismatch) PLUS a package-facts
// caveat — a graph with nodes but no rolled-up components carries no per-node package
// (built before the field, or a non-package graph), so its whole side reads as
// added/removed. Without it an old base would silently report the entire branch as new
// architecture.
func rollupDiffCaveats(base, branch *Graph, rb, rbr *PackageRollup) []string {
	caveats := provenanceCaveats(base, branch)
	if len(base.Nodes) > 0 && len(rb.Components) == 0 {
		caveats = append(caveats, "base graph carries no package facts (built before per-node package?) — every branch component reads as added")
	}
	if len(branch.Nodes) > 0 && len(rbr.Components) == 0 {
		caveats = append(caveats, "branch graph carries no package facts — every base component reads as removed")
	}
	return caveats
}

// add routes an edge into the wiring, code, or disclosure third of the diff by its
// class. Wiring is tested FIRST: a wiring edge is Resolved() too, so it would fall
// into the code bin — the very conflation this split exists to prevent.
func (d *PackageRollupDiff) add(e RollupEdge, added bool) {
	switch {
	case e.Wiring() && added:
		d.WiringAdded = append(d.WiringAdded, e)
	case e.Wiring():
		d.WiringRemoved = append(d.WiringRemoved, e)
	case e.Resolved() && added:
		d.CodeAdded = append(d.CodeAdded, e)
	case e.Resolved():
		d.CodeRemoved = append(d.CodeRemoved, e)
	case added:
		d.DisclosureAdded = append(d.DisclosureAdded, e)
	default:
		d.DisclosureRemoved = append(d.DisclosureRemoved, e)
	}
}

type rollupEdgeKey struct{ from, to, kind string }

func rollupEdgeID(e RollupEdge) rollupEdgeKey {
	return rollupEdgeKey{e.From, e.To, e.Kind}
}

func rollupEdgeSet(r *PackageRollup) map[rollupEdgeKey]bool {
	m := make(map[rollupEdgeKey]bool, len(r.Edges))
	for _, e := range r.Edges {
		m[rollupEdgeID(e)] = true
	}
	return m
}

func sortRollupEdges(es []RollupEdge) {
	sort.Slice(es, func(i, j int) bool { return rollupEdgeLess(es[i], es[j]) })
}
