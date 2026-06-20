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

// Edge-kind values. CODE = {call, effect}; DISCLOSURE = {disclosed}. The class an
// edge belongs to is its diff split, so the two are named once here.
const (
	// RollupCall is a component→component dependency: a resolved first-party call
	// crossing a package boundary. Solid.
	RollupCall = "call"
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
// first-party component — true for every kind but a call.
func (e RollupEdge) External() bool { return e.Kind != RollupCall }

// Resolved reports whether the edge is statically proven (solid) rather than a
// disclosed-but-blind effect (dashed). The diff's code-vs-disclosure split keys on it.
func (e RollupEdge) Resolved() bool { return e.Kind != RollupDisclosed }

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

	components := make([]Component, 0, len(counts))
	for pkg, c := range counts {
		components = append(components, Component{Package: pkg, Name: lastSegment(pkg), Nodes: c})
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
		seen[edgeKey{from, to, RollupCall}] = true
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

// annotationNotes indexes the human context per (site, kind): the Claim (the
// structured "what this effect does") when present, else the freeform Note. A
// disclosed component edge reads it to carry the reviewer's explanation of a blind
// effect the machine cannot prove.
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
// the first token after the "boundary:" prefix ("boundary:db SELECT applicants" → "db",
// "boundary:credit-bureau GET /score/{id}" → "credit-bureau"). The peer is the C3
// altitude for an external effect: every DB op collapses to "db", every publish to
// "bus", every call to a named peer to that peer. Returns "" for a non-boundary target
// (a first-party FQN), which is how the caller tells a call edge from an effect edge.
func boundaryPeer(to string) string {
	rest, ok := strings.CutPrefix(to, "boundary:")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		return rest[:i]
	}
	return rest
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

// PackageRollupDiff is the component delta between two rollups, split so a code change
// (a real new/dropped dependency or effect) is NEVER conflated with a disclosure change
// (a newly-documented or removed blind effect). Each list is sorted. Conflating the two
// is the failure mode this split exists to prevent: pure instrumentation (annotating a
// seam) would otherwise read as an architecture change.
type PackageRollupDiff struct {
	CodeAdded         []RollupEdge `json:"code_added"`
	CodeRemoved       []RollupEdge `json:"code_removed"`
	DisclosureAdded   []RollupEdge `json:"disclosure_added"`
	DisclosureRemoved []RollupEdge `json:"disclosure_removed"`
}

// RollupDiff computes base → branch. An edge's identity is (From, To, Kind); Note is
// presentation, so a pure note change is not a delta. The split is by edge class:
// resolved (call/effect) → code, disclosed → disclosure. Symmetric by construction —
// swapping base and branch swaps every Added list with the matching Removed.
func RollupDiff(base, branch *PackageRollup) *PackageRollupDiff {
	baseSet := rollupEdgeSet(base)
	branchSet := rollupEdgeSet(branch)
	d := &PackageRollupDiff{
		CodeAdded:         []RollupEdge{},
		CodeRemoved:       []RollupEdge{},
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
	// branch.Edges / base.Edges are already sorted, so each list is built in order; no
	// re-sort needed — but sort defensively so the contract holds regardless of input.
	sortRollupEdges(d.CodeAdded)
	sortRollupEdges(d.CodeRemoved)
	sortRollupEdges(d.DisclosureAdded)
	sortRollupEdges(d.DisclosureRemoved)
	return d
}

// add routes an edge into the code or disclosure half of the diff by its class.
func (d *PackageRollupDiff) add(e RollupEdge, added bool) {
	switch {
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
