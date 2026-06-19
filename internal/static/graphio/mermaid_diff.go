package graphio

// MermaidDiff renders the structural delta between two call graphs (base → branch)
// as a single Mermaid flowchart with the change colored in. It is the visual view
// of a comparison groundwork already performs at the JSON level — a VIEW, never a
// gate: the verdict stays with `groundwork review`, this only makes the delta
// legible to a human reviewer.
//
// The encoding is deliberately redundant so the delta is unmistakable, and — the
// design point — NODE state and EDGE state are independent, so "a function that
// still exists but lost one call" never reads the same as "a function that was
// removed". A kept node stays neutral grey even when a red removed-edge dangles off
// it; only a node that is GONE on the branch turns red. Every changed element is
// triple-cued: fill color, border style, and a +/− label prefix (so the diff
// survives greyscale printing and colorblindness).
//
// It shares the base renderer's invariants through the same helpers (collectEmitsEffect,
// keepNode, edgeDecoration, the boundary/id/label helpers) so the two renderers cannot
// drift on "an effect emitter is never hidden", boundary shaping, or edge annotation.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

type diffState int

const (
	stKept diffState = iota
	stAdded
	stRemoved
)

func stateOf(inBase, inBranch bool) diffState {
	switch {
	case inBase && inBranch:
		return stKept
	case inBranch:
		return stAdded
	default:
		return stRemoved
	}
}

// classFor maps a node/boundary state to its CSS class. A kept element gets the
// neutral grey, so the delta owns the whole color budget and a kept node can never
// be mistaken for a changed one.
func classFor(s diffState) string {
	switch s {
	case stAdded:
		return "added"
	case stRemoved:
		return "removed"
	default:
		return "kept"
	}
}

// prefixFor is the leading glyph on a changed label: a third, text-only cue beside
// fill and border, so the state reads even with no color at all.
func prefixFor(s diffState) string {
	switch s {
	case stAdded:
		return "＋ "
	case stRemoved:
		return "− "
	default:
		return ""
	}
}

type ekey struct{ from, to string }

func nodeIndex(g *Graph) map[string]Node {
	m := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.FQN] = n
	}
	return m
}

func edgeIndex(g *Graph) map[ekey]Edge {
	m := make(map[ekey]Edge, len(g.Edges))
	for _, e := range g.Edges {
		m[ekey{e.From, e.To}] = e
	}
	return m
}

// MermaidDiff renders base → branch. opts.MaxTier collapses unchanged plumbing as
// in Mermaid, but a node or edge that is part of the delta is ALWAYS shown,
// whatever its tier — the diff must never hide the very thing it is diffing.
func MermaidDiff(base, branch *Graph, opts MermaidOptions) string {
	baseN, branchN := nodeIndex(base), nodeIndex(branch)
	baseE, branchE := edgeIndex(base), edgeIndex(branch)

	// node FQN -> chosen Node (branch wins; a removed node survives only in base).
	pick := func(fqn string) Node {
		if n, ok := branchN[fqn]; ok {
			return n
		}
		return baseN[fqn]
	}
	nodeStateOf := func(fqn string) diffState {
		_, b := baseN[fqn]
		_, br := branchN[fqn]
		return stateOf(b, br)
	}

	// A node that emits a boundary effect on EITHER side is never hidden (shared rule
	// with the base renderer); any endpoint of a CHANGED edge is force-shown so the
	// delta is never collapsed away.
	emitsEffect := collectEmitsEffect(base.Edges)
	for fqn := range collectEmitsEffect(branch.Edges) {
		emitsEffect[fqn] = true
	}
	allEdges := unionEdgeKeys(baseE, branchE)
	changedEndpoint := map[string]bool{}
	for _, k := range allEdges {
		_, inB := baseE[k]
		_, inBr := branchE[k]
		if stateOf(inB, inBr) == stKept {
			continue
		}
		changedEndpoint[k.from] = true
		if !isBoundary(k.to) {
			changedEndpoint[k.to] = true
		}
	}
	// Boundary-target state, precomputed in two single passes (O(E)) rather than a
	// per-target rescan of both edge maps (the old O(boundary×edges)).
	boundaryState := boundaryTargetStates(baseE, branchE)

	ids := &idAlloc{used: map[string]bool{}}
	reserveLegendIDs(ids)

	nodeKeys := unionNodeKeys(baseN, branchN) // computed once, ranged twice below

	// Pass A: choose which first-party nodes to show, and assign ids in union order.
	// Only KEPT nodes are eligible for tier-hiding; an added/removed node is always
	// shown.
	nodeID := map[string]string{}
	shown := map[string]bool{}
	hidden := 0
	for _, fqn := range nodeKeys {
		st := nodeStateOf(fqn)
		if st == stKept && !keepNode(pick(fqn), opts.MaxTier, emitsEffect, changedEndpoint) {
			hidden++
			continue
		}
		nodeID[fqn] = ids.get(frontier.ShortName(fqn))
		shown[fqn] = true
	}
	// Above the cap a union render is an illegible hairball (a refactor PR is the
	// worst case); summarize the delta instead of drawing it.
	if opts.MaxNodes > 0 && len(shown) > opts.MaxNodes {
		return diffOverview(base, branch, opts, len(shown))
	}

	// Boundary effect nodes, once per label, in canonical edge order, with their diff
	// state from the precomputed map.
	type bnode struct {
		id, label, class string
		state            diffState
	}
	bIDs := map[string]string{}
	var bnodes []bnode
	seenBoundary := map[string]bool{}
	for _, k := range allEdges {
		if !isBoundary(k.to) || !shown[k.from] || seenBoundary[k.to] {
			continue
		}
		seenBoundary[k.to] = true
		label, class := boundaryShape(k.to)
		id := ids.get(class + "_" + label)
		bIDs[k.to] = id
		bnodes = append(bnodes, bnode{id: id, label: label, class: class, state: boundaryState[k.to]})
	}

	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("    %% call-graph diff — base → branch (a view, never a gate)\n")
	// Provenance caveats: a substrate mismatch (algo/tool) makes precision differences
	// look like code changes; disclose it so the delta is not read as confidently-wrong.
	for _, c := range provenanceCaveats(base, branch) {
		b.WriteString("    %% ⚠ " + comment(c) + "\n")
	}
	if hidden > 0 {
		b.WriteString("    %% " + plural(hidden, "unchanged node") +
			" above tier " + strconv.Itoa(opts.MaxTier) + " hidden; changed nodes are always shown\n")
	}
	writeLegend(&b)

	// First-party node declarations, in union order.
	for _, fqn := range nodeKeys {
		id, ok := nodeID[fqn]
		if !ok {
			continue
		}
		st := nodeStateOf(fqn)
		label := prefixFor(st) + mermaidText(frontier.ShortName(fqn))
		if pick(fqn).Fallible {
			label += " ⚠"
		}
		b.WriteString("    " + id + `["` + label + `"]:::` + classFor(st) + "\n")
	}
	for _, bn := range bnodes {
		open, close := boundaryDelims(bn.class)
		// Color follows the DIFF state, never the effect kind: a kept effect node is
		// neutral grey (the delta owns the color budget), a changed one recolors. The
		// shape (cylinder/hexagon/stadium) still conveys db/bus/external. Using the
		// kind class here would also reference a classDef the diff palette never defines.
		b.WriteString("    " + bn.id + open + `"` + prefixFor(bn.state) + mermaidText(bn.label) + `"` + close + ":::" + classFor(bn.state) + "\n")
	}

	// Edges, in union order; collect link indices per state for linkStyle coloring.
	var idx int
	var addedIdx, removedIdx, keptIdx []int
	for _, k := range allEdges {
		var toID string
		if isBoundary(k.to) {
			id, ok := bIDs[k.to]
			if !ok {
				continue
			}
			toID = id
		} else {
			id, ok := nodeID[k.to]
			if !ok {
				continue
			}
			toID = id
		}
		fromID, ok := nodeID[k.from]
		if !ok {
			continue
		}
		eBr, inBr := branchE[k]
		eBase, inBase := baseE[k]
		e := eBr
		if !inBr {
			e = eBase
		}
		st := stateOf(inBase, inBr)
		b.WriteString("    " + diffEdgeLine(fromID, toID, e, st) + "\n")
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

// provenanceCaveats warns when base and branch were not built on the same substrate,
// so a reviewer never reads a substrate difference as a code change. groundwork's
// JSON comparison gates on this (review/provenance.go); the visual view must at least
// DISCLOSE it — a substrate mismatch silently painted as added/removed edges would be
// exactly the confidently-wrong delta the prime directive forbids. Empty Algo/Tool on
// either side (a committed, tool-stripped golden) is treated as "unrecorded", not a
// mismatch, so a golden-vs-golden diff stays caveat-free and byte-stable.
func provenanceCaveats(base, branch *Graph) []string {
	var out []string
	if base.Algo != "" && branch.Algo != "" && base.Algo != branch.Algo {
		out = append(out, "algo differs (base "+base.Algo+" vs branch "+branch.Algo+
			"): edges differing only by analysis precision show as added/removed, not code changes")
	}
	if base.Tool != "" && branch.Tool != "" && base.Tool != branch.Tool {
		out = append(out, "producer tool differs (base "+base.Tool+" vs branch "+branch.Tool+
			"): 'same code → same graph' holds only within one tool version")
	}
	// A reclaimer (--reclaim / --reclaim-sql) ADDS provenance-tagged `via` edges that
	// a plain build never had; diffing a reclaimed branch against an un-reclaimed base
	// (or vice versa) paints those recovered edges as added/removed. The flag is not in
	// the graph, but its footprint is: any `via`-tagged edge. Disclose an asymmetry.
	if hasViaEdge(base) != hasViaEdge(branch) {
		out = append(out, "reclaimer state differs (one side carries provenance-tagged 'via' edges, "+
			"the other does not): reclaimer-recovered edges show as added/removed, not code changes")
	}
	return out
}

// hasViaEdge reports whether any edge carries a reclaimer `via` tag — the footprint
// of a --reclaim/--reclaim-sql build, used to flag a base↔branch reclaimer mismatch.
func hasViaEdge(g *Graph) bool {
	for _, e := range g.Edges {
		if e.Via != "" {
			return true
		}
	}
	return false
}

// diffOverview renders the over-cap summary: a refactor-scale delta is an illegible
// red/green hairball, so disclose the added/removed counts and the provenance caveats
// rather than drawing it. A valid, deterministic single-node diagram.
func diffOverview(base, branch *Graph, opts MermaidOptions, shownCount int) string {
	bn, brn := nodeIndex(base), nodeIndex(branch)
	var added, removed int
	for _, fqn := range unionNodeKeys(bn, brn) {
		_, inB := bn[fqn]
		_, inBr := brn[fqn]
		switch stateOf(inB, inBr) {
		case stAdded:
			added++
		case stRemoved:
			removed++
		}
	}
	ids := &idAlloc{used: map[string]bool{}}
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("    %% call-graph diff — base → branch (a view, never a gate)\n")
	for _, c := range provenanceCaveats(base, branch) {
		b.WriteString("    %% ⚠ " + comment(c) + "\n")
	}
	b.WriteString("    %% " + strconv.Itoa(shownCount) + " nodes in the union exceed the render cap (" +
		strconv.Itoa(opts.MaxNodes) + "); summarizing the delta instead\n")
	msg := "⚠ large delta — " + strconv.Itoa(added) + " added, " + strconv.Itoa(removed) +
		" removed across " + strconv.Itoa(shownCount) + " nodes. Too large to draw legibly; review the JSON diff or raise --max-nodes."
	b.WriteString("    " + ids.get("toobig") + `["` + mermaidText(msg) + `"]` + "\n")
	b.WriteString(diffClassDefs)
	return b.String()
}

// boundaryTargetStates precomputes the diff state of every boundary target in two
// single passes over the edge maps, so the per-target lookup is O(1) instead of a
// rescan of both maps per boundary node. Iteration order is irrelevant: it only fills
// a membership map keyed by the target label.
func boundaryTargetStates(baseE, branchE map[ekey]Edge) map[string]diffState {
	inBase, inBranch := map[string]bool{}, map[string]bool{}
	for k := range baseE {
		if isBoundary(k.to) {
			inBase[k.to] = true
		}
	}
	for k := range branchE {
		if isBoundary(k.to) {
			inBranch[k.to] = true
		}
	}
	out := make(map[string]diffState, len(inBase)+len(inBranch))
	for to := range inBase {
		out[to] = stateOf(true, inBranch[to])
	}
	for to := range inBranch {
		if _, ok := out[to]; !ok {
			out[to] = stateOf(false, true)
		}
	}
	return out
}

// diffEdgeLine styles an edge by its delta state: added is a thick arrow, removed a
// dotted arrow with a "removed" label, kept a plain thin arrow. The arrow SHAPE
// differs per state so the delta survives with no color (the linkStyle colors are
// an enhancement, not the only signal). Concurrency/async/via decorations from the
// underlying edge are preserved on kept and added edges via the shared edgeDecoration.
func diffEdgeLine(from, to string, e Edge, s diffState) string {
	switch s {
	case stAdded:
		text := "＋"
		if d := edgeDecoration(e); d != "" {
			text += " " + d
		}
		return from + " ==>|" + text + "| " + to
	case stRemoved:
		return from + " -.->|− removed| " + to
	default:
		if d := edgeDecoration(e); d != "" {
			return from + " -->|" + d + "| " + to
		}
		return from + " --> " + to
	}
}

func unionNodeKeys(a, b map[string]Node) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func unionEdgeKeys(a, b map[ekey]Edge) []ekey {
	seen := map[ekey]bool{}
	var out []ekey
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].from != out[j].from {
			return out[i].from < out[j].from
		}
		return out[i].to < out[j].to
	})
	return out
}

func writeLinkStyle(b *strings.Builder, idx []int, style string) {
	if len(idx) == 0 {
		return
	}
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = strconv.Itoa(n)
	}
	b.WriteString("    linkStyle " + strings.Join(parts, ",") + " " + style + "\n")
}

// reserveLegendIDs claims the fixed legend ids so the node allocator can never
// collide with them.
func reserveLegendIDs(a *idAlloc) {
	for _, id := range legendIDs {
		a.used[id] = true
	}
}

// legendIDs is the single source for the legend node ids: reserveLegendIDs claims
// them and writeLegend emits them, so the two cannot drift into a collision.
var legendIDs = []string{"lg_kept", "lg_added", "lg_removed"}

func writeLegend(b *strings.Builder) {
	b.WriteString("    subgraph legend[\"legend — base → branch\"]\n")
	b.WriteString("        direction LR\n")
	b.WriteString("        " + legendIDs[0] + "[\"unchanged\"]:::kept\n")
	b.WriteString("        " + legendIDs[1] + "[\"＋ added\"]:::added\n")
	b.WriteString("        " + legendIDs[2] + "[\"− removed\"]:::removed\n")
	b.WriteString("    end\n")
}

const diffClassDefs = "    classDef kept fill:#f6f6f6,stroke:#aaaaaa,color:#444444\n" +
	"    classDef added fill:#e7f9e7,stroke:#1a9d1a,stroke-width:2px,color:#0a5d0a\n" +
	"    classDef removed fill:#fbeaea,stroke:#cc3333,stroke-width:2px,stroke-dasharray:5 3,color:#7d0a0a\n"
