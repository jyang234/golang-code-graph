package graphio

// Mermaid renderers for the component (C3) rollup — the human-readable sibling of the
// rollup JSON, a VIEW and never a gate (like the call-graph Mermaid). The encoding
// separates the two things a reviewer must not conflate:
//   - SHAPE carries an edge's CLASS: a solid arrow is a statically resolved
//     call/effect (code), a dashed arrow is a disclosed-but-blind effect.
//   - In the diff, COLOR + a ±-prefix carry an edge's delta STATE (added/removed/kept).
//
// Shape and state are independent, so "a newly DOCUMENTED blind effect" (a dashed
// green edge) never reads the same as "a new real dependency" (a solid green edge) —
// the same code-vs-disclosure honesty the JSON diff enforces, made legible.

import (
	"sort"
	"strings"
)

// RollupMermaidOptions tunes the component (C3) rollup render. Like MermaidOptions it is
// presentation only — it never touches the rollup JSON or any verdict.
type RollupMermaidOptions struct {
	// Bands, when set, groups the component boxes into architectural BAND subgraph lanes
	// (transport / application / provisioning / storage / infrastructure / tests) read
	// from Component.Band, with the composition root drawn OUTSIDE the lanes. The zero
	// value renders the flat (dagre-scattered) layout — banding is opt-in (the CLI's
	// --rollup-bands), so the default render is byte-identical to the pre-band output and
	// a Mermaid host that dislikes subgraphs is unaffected. A VIEW grouping, NEVER a
	// gate: it reads the same name-derived band the JSON carries and changes only the
	// box LAYOUT, never the edges/topology.
	Bands bool
}

// bandRenderOrder is the canonical lane order for a banded C3 render: roughly
// entry → core → infra → tests, a stable PRESENTATION order (not a ranking and not a
// gate — see band.go). classifyBand only ever produces these six, so the list is
// exhaustive for a convention-derived band; a band outside it (a declared override
// could introduce one) is emitted after these in sorted order by bandGroups.
var bandRenderOrder = []string{
	BandTransport, BandApplication, BandProvisioning,
	BandStorage, BandInfrastructure, BandTests,
}

var bandRenderKnown = func() map[string]bool {
	m := make(map[string]bool, len(bandRenderOrder))
	for _, band := range bandRenderOrder {
		m[band] = true
	}
	return m
}()

// Mermaid renders the rollup as a Mermaid flowchart per opts. Pure, deterministic,
// begins with "flowchart LR\n" and ends with a newline. Components are boxes; external
// systems are stadium nodes; a solid arrow is a resolved call/effect, a dashed arrow a
// disclosed effect (labeled with its annotation note when one exists). With opts.Bands
// the boxes are grouped into architectural band lanes (the root drawn outside them).
func (r *PackageRollup) Mermaid(opts RollupMermaidOptions) string {
	ids := &idAlloc{used: map[string]bool{}}
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("    %% component (C3) rollup — " + plural(len(r.Components), "component") +
		", " + plural(len(r.Edges), "edge") + " (a view, never a gate)\n")
	b.WriteString("    %% solid = resolved call/effect (code); dashed = disclosed effect (blind, documented)\n")
	b.WriteString("    %% dotted into a :::root box = composition-root wiring (DI back-edge, not a domain dependency)\n")
	if opts.Bands {
		b.WriteString("    %% boxes grouped into architectural BANDS (semantic role read from the package name; a view, never a gate) — the composition root is drawn outside the bands\n")
	}

	// Allocate component ids in r.Components order (sorted) FIRST, so the id grammar is
	// byte-identical whether or not bands group the boxes: banding changes only how the
	// declarations are NESTED, never which id a package gets.
	compID := make(map[string]string, len(r.Components))
	for _, c := range r.Components {
		compID[c.Package] = ids.get("comp_" + c.Package)
	}
	if opts.Bands {
		writeBandedComponents(&b, r.Components, compID, ids)
	} else {
		for _, c := range r.Components {
			b.WriteString("    " + componentNode(c, compID[c.Package]) + "\n")
		}
	}
	extID := rollupExternalIDs(r.Edges, ids, &b)

	if len(compID) == 0 && len(extID) == 0 {
		// A node-less `flowchart LR` is not valid Mermaid; disclose the empty rollup as
		// one placeholder node instead of emitting a broken diagram.
		b.WriteString("    " + ids.get("empty") + `["(no components)"]` + "\n")
		return b.String()
	}

	var disclosedIdx, wiringIdx []int
	var idx int
	for _, e := range r.Edges {
		from, ok := compID[e.From]
		if !ok {
			continue
		}
		to, ok := rollupEndpointID(e, compID, extID)
		if !ok {
			continue
		}
		switch {
		case e.Wiring():
			// A DI back-edge into the composition root: drawn dotted, labeled "wires", so
			// it never reads as a solid domain dependency pointing the wrong way.
			b.WriteString("    " + from + " -.->|" + mermaidText("wires") + "| " + to + "\n")
			wiringIdx = append(wiringIdx, idx)
		case e.Resolved():
			b.WriteString("    " + from + " --> " + to + "\n")
		default:
			label := "discloses"
			if e.Note != "" {
				label = e.Note
			}
			b.WriteString("    " + from + " -.->|" + mermaidText(label) + "| " + to + "\n")
			disclosedIdx = append(disclosedIdx, idx)
		}
		idx++
	}
	writeLinkStyle(&b, disclosedIdx, "stroke-dasharray:5 3,stroke:#8a6d3b")
	writeLinkStyle(&b, wiringIdx, "stroke-dasharray:2 2,stroke:#8a8a8a")
	b.WriteString("    classDef ext fill:#eef3fb,stroke:#3b6ea5,color:#244a6e\n")
	b.WriteString("    classDef root fill:#f3eefb,stroke:#6b4ba5,color:#3a246e\n")
	return b.String()
}

// componentNode is one component's Mermaid node declaration (id + label + optional
// :::root class), WITHOUT the leading indent or trailing newline so the caller can nest
// it inside a band subgraph or emit it flat. The composition root carries :::root; every
// other component is an unclassed box.
func componentNode(c Component, id string) string {
	class := ""
	if c.Role == RollupRoot {
		class = ":::root" // the composition root — assembly, not a domain component
	}
	return id + `["` + mermaidText(shortPkg(c.Package)) + `"]` + class
}

// writeBandedComponents emits the component nodes grouped into architectural BAND
// subgraph lanes (transport / application / …) in the canonical band order, with the
// composition root (and any bandless component) drawn OUTSIDE the lanes. Within a band
// the boxes keep their package-sorted order (comps is r.Components, already sorted), so
// the render is deterministic. Pure presentation — the edges still reference the same
// node ids, so grouping changes the layout, never the topology.
func writeBandedComponents(b *strings.Builder, comps []Component, compID map[string]string, ids *idAlloc) {
	node := make(map[string]Component, len(comps))
	pkgs := make([]string, len(comps))
	bandOf := make(map[string]string, len(comps))
	for i, c := range comps {
		node[c.Package] = c
		pkgs[i] = c.Package
		bandOf[c.Package] = c.Band // "" for the root → drawn outside the lanes
	}
	ordered, members, bandless := bandGroups(pkgs, bandOf)
	for _, band := range ordered {
		b.WriteString("    subgraph " + ids.get("band_"+band) + `["` + mermaidText(band) + `"]` + "\n")
		for _, pkg := range members[band] {
			b.WriteString("        " + componentNode(node[pkg], compID[pkg]) + "\n")
		}
		b.WriteString("    end\n")
	}
	for _, pkg := range bandless {
		b.WriteString("    " + componentNode(node[pkg], compID[pkg]) + "\n")
	}
}

// bandGroups partitions component packages into ordered band lanes plus the bandless
// remainder (the composition root and any component with no band). It preserves each
// package's input order within a band — pkgs is the sorted node-emit order, so members
// stay deterministic — emits the known bands in bandRenderOrder, then any unknown band
// in sorted order (defensive: classifyBand never produces one, but a declared override
// could). bandOf[pkg]=="" means the package is bandless.
func bandGroups(pkgs []string, bandOf map[string]string) (ordered []string, members map[string][]string, bandless []string) {
	members = map[string][]string{}
	for _, pkg := range pkgs {
		band := bandOf[pkg]
		if band == "" {
			bandless = append(bandless, pkg)
			continue
		}
		members[band] = append(members[band], pkg)
	}
	for _, band := range bandRenderOrder {
		if len(members[band]) > 0 {
			ordered = append(ordered, band)
		}
	}
	var extra []string
	for band := range members {
		if !bandRenderKnown[band] {
			extra = append(extra, band)
		}
	}
	sort.Strings(extra)
	return append(ordered, extra...), members, bandless
}

// diffBandOf maps each component package to its band for a banded diff render, read from
// the Band the rollups already carry (one source of truth — not re-derived) and merged
// across both sides. Both sides agree on a package's band (it is a pure function of the
// name) and the root is bandless on both, so the merge order is immaterial.
func diffBandOf(base, branch *PackageRollup) map[string]string {
	bandOf := map[string]string{}
	for _, c := range base.Components {
		bandOf[c.Package] = c.Band
	}
	for _, c := range branch.Components {
		bandOf[c.Package] = c.Band
	}
	return bandOf
}

// RollupMermaidDiff renders the component delta between two GRAPHS (base → branch) per
// opts. It takes the graphs, not pre-built rollups, so it can disclose the base↔branch
// substrate skew (the provenance caveats) the same way the call-graph MermaidDiff does.
// Edge SHAPE is the class (solid resolved, dashed disclosed); fill/border/±-prefix is
// the delta state, so a newly-documented blind effect is never mistaken for a new real
// dependency. With opts.Bands the boxes are grouped into band lanes (root outside).
func RollupMermaidDiff(base, branch *Graph, opts RollupMermaidOptions) string {
	rb, rbr := base.RollupByPackage(), branch.RollupByPackage()
	return rollupMermaidDiff(rb, rbr, rollupDiffCaveats(base, branch, rb, rbr), opts)
}

// rollupMermaidDiff renders the delta between two already-built rollups, with the
// base↔branch skew caveats disclosed as header comments (the honesty channel a
// substrate-mismatched diff needs so it cannot read as a confidently-wrong delta).
func rollupMermaidDiff(base, branch *PackageRollup, caveats []string, opts RollupMermaidOptions) string {
	ids := &idAlloc{used: map[string]bool{}}
	reserveLegendIDs(ids)
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("    %% component (C3) rollup diff — base → branch (a view, never a gate)\n")
	b.WriteString("    %% solid = code (call/effect); dashed = disclosed effect; dashed \"wires\" = composition-root wiring; ＋ added, − removed\n")
	if opts.Bands {
		b.WriteString("    %% boxes grouped into architectural BANDS (semantic role; a view, never a gate) — the composition root is drawn outside the bands\n")
	}
	for _, c := range caveats {
		b.WriteString("    %% ⚠ " + comment(c) + "\n")
	}
	writeLegend(&b)

	// The composition-root packages (Role marker), for labeling the root box. A diff
	// node already carries one delta-state class (added/removed/kept), so it cannot
	// ALSO take the :::root class the non-diff view uses; the label suffix is how the
	// assembly point stays identifiable in the diff.
	roots := diffRootPackages(base, branch)

	// Component nodes in union order, colored by membership state. Ids are allocated in
	// union order FIRST so the id grammar is identical whether or not bands nest the
	// boxes (banding changes only the nesting, never which id a package gets).
	baseComp := componentSet(base)
	branchComp := componentSet(branch)
	compID := map[string]string{}
	union := unionStrings(baseComp, branchComp)
	for _, pkg := range union {
		compID[pkg] = ids.get("comp_" + pkg)
	}
	nodeLine := func(pkg string) string {
		st := stateOf(baseComp[pkg], branchComp[pkg])
		label := prefixFor(st) + mermaidText(shortPkg(pkg))
		if roots[pkg] {
			label += " " + mermaidText("(root)")
		}
		return compID[pkg] + `["` + label + `"]:::` + classFor(st)
	}
	if opts.Bands {
		ordered, members, bandless := bandGroups(union, diffBandOf(base, branch))
		for _, band := range ordered {
			b.WriteString("    subgraph " + ids.get("band_"+band) + `["` + mermaidText(band) + `"]` + "\n")
			for _, pkg := range members[band] {
				b.WriteString("        " + nodeLine(pkg) + "\n")
			}
			b.WriteString("    end\n")
		}
		for _, pkg := range bandless {
			b.WriteString("    " + nodeLine(pkg) + "\n")
		}
	} else {
		for _, pkg := range union {
			b.WriteString("    " + nodeLine(pkg) + "\n")
		}
	}

	// External nodes in union edge order, colored by membership state.
	baseExt := externalStates(base)
	branchExt := externalStates(branch)
	extID := map[string]string{}
	for _, ext := range unionStrings(baseExt, branchExt) {
		id := ids.get("ext_" + ext)
		extID[ext] = id
		st := stateOf(baseExt[ext], branchExt[ext])
		b.WriteString("    " + id + `(["` + prefixFor(st) + mermaidText(shortPkg(ext)) + `"]):::` + classFor(st) + "\n")
	}

	// Edges in union order; color by state via linkStyle, shape by class.
	baseE := rollupEdgeSet(base)
	branchE := rollupEdgeSet(branch)
	var addedIdx, removedIdx, keptIdx []int
	var idx int
	for _, e := range unionRollupEdges(base, branch) {
		from, ok := compID[e.From]
		if !ok {
			continue
		}
		to, ok := rollupEndpointID(e, compID, extID)
		if !ok {
			continue
		}
		st := stateOf(baseE[rollupEdgeID(e)], branchE[rollupEdgeID(e)])
		b.WriteString("    " + rollupDiffEdgeLine(from, to, e, st) + "\n")
		switch st {
		case stAdded:
			addedIdx = append(addedIdx, idx)
		case stRemoved:
			removedIdx = append(removedIdx, idx)
		default:
			keptIdx = append(keptIdx, idx)
		}
		idx++
	}
	writeLinkStyle(&b, addedIdx, "stroke:#1a9d1a,stroke-width:3px")
	writeLinkStyle(&b, removedIdx, "stroke:#cc3333,stroke-width:2px")
	writeLinkStyle(&b, keptIdx, "stroke:#cccccc")
	b.WriteString(diffClassDefs)
	return b.String()
}

// rollupDiffEdgeLine styles a component edge: a dashed arrow for a disclosed effect OR
// a composition-root wiring back-edge, a solid one for resolved domain code, with a
// ±-prefix label so the delta survives greyscale. Wiring is drawn dashed like a
// disclosure (not solid like a call) so a re-wired injection never reads as a new/
// dropped real dependency — the same code-vs-wiring honesty the JSON diff enforces.
func rollupDiffEdgeLine(from, to string, e RollupEdge, s diffState) string {
	arrow := "-->"
	if !e.Resolved() || e.Wiring() {
		arrow = "-.->"
	}
	// The label carries the delta state (±) AND, for a wiring back-edge, a "wires"
	// tag. Both wiring and disclosed edges are dashed, and the delta COLOR is shared
	// across classes, so without the tag a kept wiring edge and a kept disclosed edge
	// would render byte-identically — losing the code-vs-wiring distinction the JSON
	// diff keeps. (A second linkStyle for the wiring class is not available: the edge
	// index already carries its delta-state linkStyle.)
	var parts []string
	if p := strings.TrimSpace(prefixFor(s)); p != "" {
		parts = append(parts, p)
	}
	if e.Wiring() {
		parts = append(parts, "wires")
	}
	if len(parts) > 0 {
		return from + " " + arrow + "|" + mermaidText(strings.Join(parts, " ")) + "| " + to
	}
	return from + " " + arrow + " " + to
}

// diffRootPackages is the set of composition-root packages across either side of a
// rollup diff — read from the Component.Role marker each side already carries.
func diffRootPackages(base, branch *PackageRollup) map[string]bool {
	roots := map[string]bool{}
	for _, c := range base.Components {
		if c.Role == RollupRoot {
			roots[c.Package] = true
		}
	}
	for _, c := range branch.Components {
		if c.Role == RollupRoot {
			roots[c.Package] = true
		}
	}
	return roots
}

// rollupEndpointID resolves an edge's To to a node id: a component for a call, an
// external system otherwise.
func rollupEndpointID(e RollupEdge, compID, extID map[string]string) (string, bool) {
	if e.External() {
		id, ok := extID[e.To]
		return id, ok
	}
	id, ok := compID[e.To]
	return id, ok
}

// rollupExternalIDs assigns an id and emits (to b) a stadium node for each distinct
// external system referenced by an effect/disclosed edge, in sorted order
// (deterministic), returning the system→id map the edge pass resolves To against.
func rollupExternalIDs(edges []RollupEdge, ids *idAlloc, b *strings.Builder) map[string]string {
	set := map[string]bool{}
	for _, e := range edges {
		if e.External() {
			set[e.To] = true
		}
	}
	ext := make([]string, 0, len(set))
	for e := range set {
		ext = append(ext, e)
	}
	sort.Strings(ext)
	out := make(map[string]string, len(ext))
	for _, e := range ext {
		id := ids.get("ext_" + e)
		out[e] = id
		b.WriteString("    " + id + `(["` + mermaidText(shortPkg(e)) + `"]):::ext` + "\n")
	}
	return out
}

// componentSet is the set of component package ids in a rollup.
func componentSet(r *PackageRollup) map[string]bool {
	m := make(map[string]bool, len(r.Components))
	for _, c := range r.Components {
		m[c.Package] = true
	}
	return m
}

// externalStates is the set of external-system ids referenced by a rollup's edges.
func externalStates(r *PackageRollup) map[string]bool {
	m := map[string]bool{}
	for _, e := range r.Edges {
		if e.External() {
			m[e.To] = true
		}
	}
	return m
}

func unionStrings(a, b map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionRollupEdges(base, branch *PackageRollup) []RollupEdge {
	seen := map[rollupEdgeKey]RollupEdge{}
	for _, e := range base.Edges {
		seen[rollupEdgeID(e)] = e
	}
	for _, e := range branch.Edges {
		seen[rollupEdgeID(e)] = e // branch wins (its Note, if any)
	}
	out := make([]RollupEdge, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sortRollupEdges(out)
	return out
}
