package graphio

// Mermaid renders the non-gated call-graph view as a Mermaid flowchart — the
// human-readable sibling of Marshal's JSON, the way render.Mermaid is the
// human-readable sibling of a trace golden. It is a pure, deterministic function
// of the Graph (itself already canonically sorted by Build), so renderer drift
// never pollutes a gate: like the JSON view this is "what can happen" for human
// understanding, never a verdict.
//
// Where the sequence diagram shows one observed flow, this shows the static
// over-approximation: the first-party calls reachable from the scope, the typed
// boundary effects (DB / bus / external) as shaped leaf nodes, and — the thing no
// behavioral diagram can offer — the blind spots and frontier markers as explicit
// terminal nodes, so a reviewer sees exactly where the analysis STOPS seeing
// instead of mistaking an incomplete graph for a complete one (CLAUDE.md fail
// closed / self-honesty about blind spots). Above a node cap (MermaidOptions.MaxNodes)
// the full graph is illegible, so the render becomes an INDEX of entry points to
// scope at — but the blind-spot / frontier disclosures are still drawn, so the
// honesty channel survives the cap.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/render"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// MermaidOptions tunes the flowchart render.
type MermaidOptions struct {
	// MaxTier, when > 0, hides first-party nodes whose salience tier EXCEEDS it —
	// the low-salience plumbing (telemetry, compute-only closures) that buries the
	// decisions and effects a reviewer is looking for. The hide is sound-by-omission
	// only: a node that EMITS a boundary edge, or that is the source of a shown call,
	// is never hidden, so the diagram never silently drops an effect; the count of
	// what was hidden is disclosed in a header comment. The zero value (0) hides
	// nothing — the library renders the complete graph by default, and the CLI opts
	// into denoising for humans.
	MaxTier int

	// MaxNodes, when > 0, caps how many first-party nodes a single diagram will draw.
	// A real service's whole-graph render is an illegible hairball (and exceeds the
	// node limits some Mermaid hosts enforce), so above the cap the renderer emits an
	// INDEX instead — the entry points to scope at with --root — rather than a broken
	// diagram. The zero value (0) is uncapped, so the library renders everything by
	// default and the CLI opts into a legible cap for humans.
	MaxNodes int
}

// Mermaid renders g as a Mermaid flowchart per opts. The output begins with
// "flowchart LR\n" and ends with a newline, and is byte-identical across runs for
// a given (g, opts).
func (g *Graph) Mermaid(opts MermaidOptions) string {
	return g.mermaid(opts, nil)
}

// bnode is a typed boundary effect node (DB / bus / external leaf). disc is a
// disclosure node (a blind spot or frontier marker, the honesty channel); from is the
// id of the first-party node it attaches to, or "" when that node is not drawn.
type (
	bnode struct{ id, label, class string }
	disc  struct{ id, label, from string }
)

// writeFlowchartHeader emits the shared header — the `flowchart LR` line and the
// `%% scope … algo` comment — for every static-graph view (full render and the
// over-cap index alike), so the header format lives in ONE place (CLAUDE.md: one
// source of truth) and the index can never drift from the full render's header.
func writeFlowchartHeader(b *strings.Builder, scope, algo string) {
	b.WriteString("flowchart LR\n")
	if scope == "" {
		scope = "whole graph"
	}
	b.WriteString("    %% static call graph — scope: " + comment(scope) + "; algo: " + comment(algo) + "\n")
}

// buildBoundaryNodes assigns ids to the distinct boundary effect nodes reached from a
// shown source, in canonical edge order. Shared so the boundary set is built once.
func buildBoundaryNodes(g *Graph, ids *idAlloc, shown map[string]bool) (map[string]string, []bnode) {
	bIDs := map[string]string{}
	var bnodes []bnode
	for _, to := range orderedBoundaryTargets(g.Edges, shown) {
		label, class := boundaryShape(to)
		id := ids.get(class + "_" + label)
		bIDs[to] = id
		bnodes = append(bnodes, bnode{id: id, label: label, class: class})
	}
	return bIDs, bnodes
}

// buildDiscs assigns ids to the disclosure nodes (blind spots then frontier markers),
// attaching each to its first-party site/owner when that node is drawn. It is the one
// builder of the honesty channel, used by BOTH the full render and the over-cap index,
// so a large graph can never silently drop a blind spot the small graph would show.
// annotationNotes renders the human/AI context lines for the header, one per
// annotation in the graph's canonical (Site, Kind) order. Disclosure only — the
// note explains a blind spot, never closes it.
func annotationNotes(g *Graph) []string {
	out := make([]string, 0, len(g.Annotations))
	for _, a := range g.Annotations {
		line := "annotation · " + a.Kind + " at " + frontier.ShortName(a.Site) + ": " + a.Note
		if a.By != "" {
			line += " — by " + a.By
		}
		out = append(out, line)
	}
	return out
}

// blindSpotLabel is the disclosure-node caption. Every blind spot reads
// "⊥ <kind><br/>blind spot"; an ExternalBoundaryCall instead names its target package
// and signal/noise tier, so a graph's many boundary nodes are not one
// indistinguishable "⊥ ExternalBoundaryCall blind spot" box — the SNS-publish seam
// reads apart from the uuid seam at a glance (§21.B). The tier (Severity, a structural
// field) is the reliable distinguisher; the package is a best-effort enrichment parsed
// from the Detail prose the producer owns, so a parse miss degrades to the tier alone,
// never to a silent collision.
func blindSpotLabel(b blindspots.BlindSpot) string {
	base := "⊥ " + mermaidText(string(b.Kind))
	if b.Kind != blindspots.ExternalBoundaryCall {
		return base + "<br/>blind spot"
	}
	var parts []string
	if pkg := frontier.ShortName(blindspots.ExternalPackage(b.Detail)); pkg != "" {
		parts = append(parts, "→ "+pkg)
	}
	if b.Severity != "" {
		parts = append(parts, string(b.Severity))
	}
	if len(parts) == 0 {
		return base + "<br/>blind spot"
	}
	return base + "<br/>" + mermaidText(strings.Join(parts, " · "))
}

func buildDiscs(g *Graph, ids *idAlloc, nodeID map[string]string) []disc {
	annotated := map[[2]string]bool{}
	for _, a := range g.Annotations {
		annotated[[2]string{a.Site, a.Kind}] = true
	}
	var discs []disc
	for _, b := range g.BlindSpots {
		id := ids.get("blind_" + string(b.Kind))
		label := blindSpotLabel(b)
		if annotated[[2]string{b.Site, string(b.Kind)}] {
			// A blind spot a human/AI has supplied context for: marked so a reader
			// sees "explained" vs "unexamined" at a glance; the note text rides the
			// header notes (mermaid), keeping the node label legible.
			label += " 🗒"
		}
		discs = append(discs, disc{id: id, label: label, from: nodeID[b.Site]})
	}
	if g.Frontier != nil {
		for _, m := range g.Frontier.Markers {
			id := ids.get("frontier_" + m.Kind)
			label := "⌖ " + mermaidText(m.Kind) + "<br/>frontier " + mermaidText(string(m.Bin))
			discs = append(discs, disc{id: id, label: label, from: nodeID[m.Owner]})
		}
	}
	return discs
}

// mermaid is the shared renderer. notes are extra %% disclosure lines emitted in
// the header (after the hidden-plumbing note) — MermaidRootedAt uses them to
// disclose what a per-handler scoping pruned. Above opts.MaxNodes (counting the FULL
// drawn set — first-party + boundary effects + disclosures, not first-party alone)
// it returns an index of entry points instead of an illegible hairball.
func (g *Graph) mermaid(opts MermaidOptions, notes []string) string {
	ids := &idAlloc{used: map[string]bool{}}

	// Annotation context rides the header disclosure notes (deterministic, already
	// sorted by Site/Kind), so it survives the node cap (the overview gets the same
	// notes) and stays legible instead of crammed into a node label.
	notes = append(notes, annotationNotes(g)...)

	// A node that emits a boundary effect is load-bearing: hiding it would drop the
	// effect from the diagram, which the trust model forbids (keepNode enforces this).
	emitsEffect := collectEmitsEffect(g.Edges)

	// Pass A: assign ids to the first-party nodes we will show.
	nodeID := make(map[string]string, len(g.Nodes))
	shown := make(map[string]bool, len(g.Nodes))
	hidden := 0
	for _, n := range g.Nodes {
		if !keepNode(n, opts.MaxTier, emitsEffect, nil) {
			hidden++
			continue
		}
		nodeID[n.FQN] = ids.get(frontier.ShortName(n.FQN))
		shown[n.FQN] = true
	}

	bIDs, bnodes := buildBoundaryNodes(g, ids, shown)
	discs := buildDiscs(g, ids, nodeID)

	// Cap on the FULL drawn-node count — first-party + boundary effects + disclosures.
	// Counting first-party alone under-counts: a thin handler over many distinct
	// effects (or many blind spots) stays "under cap" yet still draws a hairball and
	// can exceed a Mermaid host's node limit. The disclosures still ride the index, so
	// the honesty channel survives the cap.
	if opts.MaxNodes > 0 && len(shown)+len(bnodes)+len(discs) > opts.MaxNodes {
		return g.overview(opts, ids, len(shown)+len(bnodes)+len(discs), discs, notes)
	}

	var b strings.Builder
	writeFlowchartHeader(&b, g.Entrypoint, g.Algo)
	if hidden > 0 {
		b.WriteString("    %% " + plural(hidden, "first-party node") +
			" above tier " + strconv.Itoa(opts.MaxTier) + " hidden as plumbing; pass --show-plumbing to include\n")
	}
	for _, n := range notes {
		b.WriteString("    %% " + comment(n) + "\n")
	}

	// First-party node declarations, in canonical Node order.
	for _, n := range g.Nodes {
		id, ok := nodeID[n.FQN]
		if !ok {
			continue
		}
		label := mermaidText(frontier.ShortName(n.FQN))
		if n.Fallible {
			label += " ⚠"
		}
		decl := "    " + id + `["` + label + `"]`
		if n.Fallible {
			decl += ":::fallible"
		}
		b.WriteString(decl + "\n")
	}
	for _, bn := range bnodes {
		open, close := boundaryDelims(bn.class)
		b.WriteString("    " + bn.id + open + `"` + mermaidText(bn.label) + `"` + close + ":::" + bn.class + "\n")
	}
	for _, d := range discs {
		b.WriteString("    " + d.id + `(["` + d.label + `"]):::blind` + "\n")
	}
	// A flowchart with no nodes is not valid Mermaid (a node-less `flowchart LR`
	// renders as an error). A whole-graph render of an empty/library graph, or a
	// scope that filtered everything, hits this — emit a placeholder so the output
	// stays a valid, self-explaining diagram rather than a broken one.
	if len(nodeID) == 0 && len(bnodes) == 0 && len(discs) == 0 {
		b.WriteString(`    empty["(no nodes in scope)"]` + "\n")
	}

	// Edges, in canonical Edge order. A boundary edge draws to its effect node; a
	// first-party edge draws only when both endpoints are shown.
	for _, e := range g.Edges {
		if isBoundary(e.To) {
			// Both endpoints must be drawn: the source must be a SHOWN first-party node
			// (a boundary target can be reached from an unshown/dangling source when
			// another shown node also reaches it), else the edge would have an empty
			// source id — invalid Mermaid.
			from, okF := nodeID[e.From]
			to, okT := bIDs[e.To]
			if !okF || !okT {
				continue
			}
			b.WriteString("    " + edgeLine(from, to, e) + "\n")
			continue
		}
		from, okF := nodeID[e.From]
		to, okT := nodeID[e.To]
		if !okF || !okT {
			continue
		}
		b.WriteString("    " + edgeLine(from, to, e) + "\n")
	}
	// Disclosure attachments: a dashed link from the attributed node to its blind
	// spot / frontier marker, so the reviewer reads it as "here the graph goes dark".
	for _, d := range discs {
		if d.from == "" {
			continue
		}
		b.WriteString("    " + d.from + " -. blind .-> " + d.id + "\n")
	}

	b.WriteString(classDefs)
	return b.String()
}

// overview renders the over-cap INDEX: rather than an illegible whole-graph
// hairball, it discloses the size and — for an unscoped graph — lists the entry
// points a reviewer can scope to with --root, so the too-big case stays a useful,
// valid, deterministic diagram instead of a broken one. A scoped graph that is still
// too big (one fan-out-heavy handler) steers to raising the cap or narrowing instead.
// The disclosure nodes are ALWAYS drawn here too (passed in by the caller), so the
// honesty channel is never dropped just because the graph crossed the cap. It shares
// the caller's id allocator so the index's ids cannot collide with the disclosures'.
func (g *Graph) overview(opts MermaidOptions, ids *idAlloc, drawn int, discs []disc, notes []string) string {
	var b strings.Builder
	writeFlowchartHeader(&b, g.Entrypoint, g.Algo)
	b.WriteString("    %% " + strconv.Itoa(drawn) + " nodes exceed the render cap (" +
		strconv.Itoa(opts.MaxNodes) + "); rendering an index instead — scope with --root or raise --max-nodes\n")
	for _, n := range notes {
		b.WriteString("    %% " + comment(n) + "\n")
	}

	root := ids.get("toobig")
	if g.Entrypoint == "" && len(g.Entrypoints) > 0 {
		b.WriteString("    " + root + `["` +
			mermaidText("⚠ "+strconv.Itoa(drawn)+" nodes — too large to draw legibly. Scope to one entry point:") + `"]` + "\n")
		eps := append([]Entrypoint(nil), g.Entrypoints...)
		// (Name, Fn, Kind) is a TOTAL order, so sort.Slice (which is not stable) cannot
		// permute entries that tie on a prefix — the index bytes stay deterministic even
		// when two entry points share a (Name, Fn).
		sort.Slice(eps, func(i, j int) bool {
			if eps[i].Name != eps[j].Name {
				return eps[i].Name < eps[j].Name
			}
			if eps[i].Fn != eps[j].Fn {
				return eps[i].Fn < eps[j].Fn
			}
			return eps[i].Kind < eps[j].Kind
		})
		const maxList = 60
		for i, e := range eps {
			if i >= maxList {
				more := ids.get("more")
				b.WriteString("    " + root + " --> " + more + `["` +
					mermaidText("… and "+strconv.Itoa(len(eps)-maxList)+" more entry points") + `"]` + "\n")
				break
			}
			b.WriteString("    " + root + " --> " + ids.get("ep_"+e.Name) + `["` + mermaidText(e.Name) + `"]` + "\n")
		}
	} else {
		b.WriteString("    " + root + `["` +
			mermaidText("⚠ "+strconv.Itoa(drawn)+" nodes in this scope — too large to draw legibly. Narrow the scope, or raise --max-nodes to render anyway.") + `"]` + "\n")
	}
	// The honesty channel survives the cap: blind spots and frontier markers are drawn
	// standalone (the first-party nodes they attach to are not in the index), so a large
	// graph's "where the analysis goes dark" markers are never silently dropped.
	for _, d := range discs {
		b.WriteString("    " + d.id + `(["` + d.label + `"]):::blind` + "\n")
	}
	b.WriteString(classDefs)
	return b.String()
}

const classDefs = "    classDef fallible stroke:#c44,stroke-width:2px\n" +
	"    classDef db fill:#eef,stroke:#66a\n" +
	"    classDef bus fill:#efe,stroke:#5a5\n" +
	"    classDef external fill:#fef6e8,stroke:#c93\n" +
	"    classDef blind fill:#fde,stroke:#c33,stroke-dasharray:3 3\n"

// mermaidText neutralizes DATA-derived label text so it cannot corrupt the diagram.
// Two hazards: (1) Mermaid renders the text between a node's quotes as HTML — we rely
// on that for the literal <br/> the disclosure labels inject — so a '<'/'>'/'"' from
// the data (the "<dynamic>" effect marker, a generic type parameter, an embedded
// quote) would be parsed as markup and silently dropped or break the quoting; the five
// HTML-significant characters are escaped so data stays literal while our own <br/>
// stays markup. (2) A raw newline/carriage-return in the data (a multi-line SQL label,
// a crafted FQN) would split the single-line node declaration into two, producing
// invalid Mermaid — so control characters are folded to a space. Applied to data text
// only, never to the markup we compose around it.
func mermaidText(s string) string {
	return labelEscaper.Replace(s)
}

var labelEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
	"\n", " ",
	"\r", " ",
	"\t", " ",
)

// collectEmitsEffect returns the set of first-party FQNs that emit at least one
// boundary effect. Such a node is load-bearing — hiding it would drop the effect —
// so keepNode never collapses it. The single source of this rule for both the base
// renderer and the diff renderer (CLAUDE.md: one source of truth).
func collectEmitsEffect(edges []Edge) map[string]bool {
	m := map[string]bool{}
	for _, e := range edges {
		if isBoundary(e.To) {
			m[e.From] = true
		}
	}
	return m
}

// keepNode reports whether node n is shown under the tier filter: a node is hidden
// only when it is plumbing above maxTier AND emits no boundary effect AND is not
// force-shown (force carries the diff's changed endpoints). force may be nil. This is
// the one tier-hide predicate both renderers share, so the soundness rule "an effect
// emitter is never hidden" cannot drift between them.
func keepNode(n Node, maxTier int, emitsEffect, force map[string]bool) bool {
	if maxTier <= 0 || n.Tier <= maxTier {
		return true
	}
	return emitsEffect[n.FQN] || force[n.FQN]
}

// orderedBoundaryTargets returns the distinct boundary "to" labels reached from a
// shown source, in canonical edge order so id assignment is deterministic. Shared by
// both renderers, so the "once per label, source must be shown" invariant lives once.
func orderedBoundaryTargets(edges []Edge, shown map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range edges {
		if isBoundary(e.To) && shown[e.From] && !seen[e.To] {
			seen[e.To] = true
			out = append(out, e.To)
		}
	}
	return out
}

// isBoundary reports whether a To label is a typed boundary effect node rather
// than a first-party callee. Build emits these with the "boundary:" prefix.
func isBoundary(to string) bool { return strings.HasPrefix(to, "boundary:") }

// boundaryShape returns the display label (the part after "boundary:") and the
// CSS class keyed off the effect kind: bus, db, or an external host.
func boundaryShape(to string) (label, class string) {
	label = strings.TrimPrefix(to, "boundary:")
	switch {
	case strings.HasPrefix(label, "bus "):
		return label, "bus"
	case strings.HasPrefix(label, "db "):
		return label, "db"
	default:
		return label, "external"
	}
}

// boundaryDelims gives the Mermaid node-shape delimiters per effect class: bus a
// hexagon, db a cylinder, external a stadium — mirroring render.SystemGraph so the
// two flowcharts read with one visual vocabulary.
func boundaryDelims(class string) (open, close string) {
	switch class {
	case "bus":
		return "{{", "}}"
	case "db":
		return "[(", ")]"
	default:
		return "([", "])"
	}
}

// edgeDecoration is the non-diff annotation an edge carries: go/async for a
// concurrent or asynchronous hop, and the reclaimer `via` provenance. It is the one
// source of edge-annotation text for both edgeLine and the diff renderer, so a new
// annotation cannot appear on one and not the other (CLAUDE.md: one source of truth).
// The output is edge-label-safe: the `via` value is data, so it is HTML-escaped and
// its '|' (the Mermaid edge-label delimiter) neutralized, just as node labels are.
func edgeDecoration(e Edge) string {
	var text string
	switch {
	case e.Concurrent:
		text = "go"
	case e.Boundary == "outbound-async":
		text = "async"
	}
	if e.Via != "" {
		if text != "" {
			text += "; "
		}
		text += "via " + e.Via
	}
	return edgeLabelSafe(text)
}

// edgeLabelSafe makes text safe to place in a Mermaid edge label. Beyond the node-label
// escaping (HTML-significant chars, control chars), it neutralizes '|' — the character
// that DELIMITS a `-->|label|` edge label, so a '|' in the data would close the label
// early and corrupt the edge. Folded to '/' rather than escaped, since edge labels are
// short provenance, not prose.
func edgeLabelSafe(text string) string {
	return strings.ReplaceAll(mermaidText(text), "|", "/")
}

// edgeLine renders one edge. Concurrent (`go`) and outbound-async hops are dashed
// to set them apart from synchronous calls (the flowchart analogue of the sequence
// renderer's `--)` async arrow); a reclaimed edge carries its `via` provenance as
// the link label so a reviewer can see which seam-reclaimer recovered it.
func edgeLine(from, to string, e Edge) string {
	dashed := e.Concurrent || e.Boundary == "outbound-async"
	text := edgeDecoration(e)
	if dashed {
		if text == "" {
			return from + " -.-> " + to
		}
		return from + " -. " + text + " .-> " + to
	}
	if text == "" {
		return from + " --> " + to
	}
	return from + " -->|" + text + "| " + to
}

// comment sanitizes a string for use inside a Mermaid %% comment: newlines would
// terminate the comment line and corrupt the diagram, so they are folded to spaces.
func comment(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
}

// idAlloc hands out Mermaid-safe, collision-free node ids from human seeds,
// deterministically, through render's shared id grammar (render.SanitizeID /
// render.UniqueID) — the one Mermaid-id convention every view in the codebase uses,
// rather than a per-package copy that could drift (CLAUDE.md: one source of truth).
type idAlloc struct{ used map[string]bool }

func (a *idAlloc) get(seed string) string {
	return render.UniqueID(render.SanitizeID(seed), a.used)
}

func plural(n int, noun string) string {
	s := strconv.Itoa(n) + " " + noun
	if n != 1 {
		s += "s"
	}
	return s
}
