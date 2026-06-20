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

// Mermaid renders the rollup as a Mermaid flowchart. Pure, deterministic, begins with
// "flowchart LR\n" and ends with a newline. Components are boxes; external systems are
// stadium nodes; a solid arrow is a resolved call/effect, a dashed arrow a disclosed
// effect (labeled with its annotation note when one exists).
func (r *PackageRollup) Mermaid() string {
	ids := &idAlloc{used: map[string]bool{}}
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("    %% component (C3) rollup — " + plural(len(r.Components), "component") +
		", " + plural(len(r.Edges), "edge") + " (a view, never a gate)\n")
	b.WriteString("    %% solid = resolved call/effect (code); dashed = disclosed effect (blind, documented)\n")

	compID := make(map[string]string, len(r.Components))
	for _, c := range r.Components {
		id := ids.get("comp_" + c.Package)
		compID[c.Package] = id
		b.WriteString("    " + id + `["` + mermaidText(shortPkg(c.Package)) + `"]` + "\n")
	}
	extID := rollupExternalIDs(r.Edges, ids, &b)

	if len(compID) == 0 && len(extID) == 0 {
		// A node-less `flowchart LR` is not valid Mermaid; disclose the empty rollup as
		// one placeholder node instead of emitting a broken diagram.
		b.WriteString("    " + ids.get("empty") + `["(no components)"]` + "\n")
		return b.String()
	}

	var disclosedIdx []int
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
		if e.Resolved() {
			b.WriteString("    " + from + " --> " + to + "\n")
		} else {
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
	b.WriteString("    classDef ext fill:#eef3fb,stroke:#3b6ea5,color:#244a6e\n")
	return b.String()
}

// RollupMermaidDiff renders the component delta between two GRAPHS (base → branch). It
// takes the graphs, not pre-built rollups, so it can disclose the base↔branch substrate
// skew (the provenance caveats) the same way the call-graph MermaidDiff does. Edge SHAPE
// is the class (solid resolved, dashed disclosed); fill/border/±-prefix is the delta
// state, so a newly-documented blind effect is never mistaken for a new real dependency.
func RollupMermaidDiff(base, branch *Graph) string {
	rb, rbr := base.RollupByPackage(), branch.RollupByPackage()
	return rollupMermaidDiff(rb, rbr, rollupDiffCaveats(base, branch, rb, rbr))
}

// rollupMermaidDiff renders the delta between two already-built rollups, with the
// base↔branch skew caveats disclosed as header comments (the honesty channel a
// substrate-mismatched diff needs so it cannot read as a confidently-wrong delta).
func rollupMermaidDiff(base, branch *PackageRollup, caveats []string) string {
	ids := &idAlloc{used: map[string]bool{}}
	reserveLegendIDs(ids)
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("    %% component (C3) rollup diff — base → branch (a view, never a gate)\n")
	b.WriteString("    %% solid = code (call/effect); dashed = disclosed effect; ＋ added, − removed\n")
	for _, c := range caveats {
		b.WriteString("    %% ⚠ " + comment(c) + "\n")
	}
	writeLegend(&b)

	// Component nodes in union order, colored by membership state.
	baseComp := componentSet(base)
	branchComp := componentSet(branch)
	compID := map[string]string{}
	for _, pkg := range unionStrings(baseComp, branchComp) {
		id := ids.get("comp_" + pkg)
		compID[pkg] = id
		st := stateOf(baseComp[pkg], branchComp[pkg])
		b.WriteString("    " + id + `["` + prefixFor(st) + mermaidText(shortPkg(pkg)) + `"]:::` + classFor(st) + "\n")
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

// rollupDiffEdgeLine styles a component edge: a dashed arrow for a disclosed effect, a
// solid one for resolved code, with a ±-prefix label so the delta survives greyscale.
func rollupDiffEdgeLine(from, to string, e RollupEdge, s diffState) string {
	arrow := "-->"
	if !e.Resolved() {
		arrow = "-.->"
	}
	if p := strings.TrimSpace(prefixFor(s)); p != "" {
		return from + " " + arrow + "|" + p + "| " + to
	}
	return from + " " + arrow + " " + to
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
