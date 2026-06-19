package graphio

// Mermaid renders the non-gated call-graph view as a Mermaid flowchart — the
// human-readable sibling of Marshal's JSON, the way render.Mermaid is the
// human-readable sibling of a trace golden. It is a pure, deterministic function
// of the Graph (itself already canonically sorted by Build), so renderer drift
// never pollutes a gate: like the JSON view this is "what can happen" for human
// understanding, never a verdict.
//
// Where the sequence diagram shows one observed flow, this shows the static
// over-approximation: every first-party call reachable from the scope, the typed
// boundary effects (DB / bus / external) as shaped leaf nodes, and — the thing no
// behavioral diagram can offer — the blind spots and frontier markers as explicit
// terminal nodes, so a reviewer sees exactly where the analysis STOPS seeing
// instead of mistaking an incomplete graph for a complete one (CLAUDE.md fail
// closed / self-honesty about blind spots).

import (
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/render"
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
}

// Mermaid renders g as a Mermaid flowchart per opts. The output begins with
// "flowchart LR\n" and ends with a newline, and is byte-identical across runs for
// a given (g, opts).
func (g *Graph) Mermaid(opts MermaidOptions) string {
	return g.mermaid(opts, nil)
}

// mermaid is the shared renderer. notes are extra %% disclosure lines emitted in
// the header (after the hidden-plumbing note) — MermaidRootedAt uses them to
// disclose what a per-handler scoping pruned.
func (g *Graph) mermaid(opts MermaidOptions, notes []string) string {
	ids := &idAlloc{used: map[string]bool{}}

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

	// Boundary effect nodes, created once per distinct label, in canonical edge order
	// and only when their source is shown.
	type bnode struct {
		id, label, class string
	}
	bIDs := map[string]string{}
	var bnodes []bnode
	for _, to := range orderedBoundaryTargets(g.Edges, shown) {
		label, class := boundaryShape(to)
		id := ids.get(class + "_" + label)
		bIDs[to] = id
		bnodes = append(bnodes, bnode{id: id, label: label, class: class})
	}

	// Disclosure nodes: graph-completeness blind spots and frontier markers, the
	// honesty channel. Each hangs off the first-party node it is attributed to (its
	// site / owner) when that node is shown, else stands alone — but it is ALWAYS
	// drawn, so the diagram cannot launder a blind spot into clean reachability.
	type disc struct {
		id, label, from string // from = source node id, or "" for a standalone
	}
	var discs []disc
	for _, b := range g.BlindSpots {
		id := ids.get("blind_" + string(b.Kind))
		discs = append(discs, disc{id: id, label: "⊥ " + mermaidText(string(b.Kind)) + "<br/>blind spot", from: nodeID[b.Site]})
	}
	if g.Frontier != nil {
		for _, m := range g.Frontier.Markers {
			id := ids.get("frontier_" + m.Kind)
			label := "⌖ " + mermaidText(m.Kind) + "<br/>frontier " + mermaidText(string(m.Bin))
			discs = append(discs, disc{id: id, label: label, from: nodeID[m.Owner]})
		}
	}

	var b strings.Builder
	b.WriteString("flowchart LR\n")
	scope := g.Entrypoint
	if scope == "" {
		scope = "whole graph"
	}
	b.WriteString("    %% static call graph — scope: " + comment(scope) + "; algo: " + comment(g.Algo) + "\n")
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
			to, ok := bIDs[e.To]
			if !ok {
				continue
			}
			b.WriteString("    " + edgeLine(nodeID[e.From], to, e) + "\n")
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

const classDefs = "    classDef fallible stroke:#c44,stroke-width:2px\n" +
	"    classDef db fill:#eef,stroke:#66a\n" +
	"    classDef bus fill:#efe,stroke:#5a5\n" +
	"    classDef external fill:#fef6e8,stroke:#c93\n" +
	"    classDef blind fill:#fde,stroke:#c33,stroke-dasharray:3 3\n"

// mermaidText escapes DATA-derived label text for Mermaid's HTML-label mode. Mermaid
// renders the text between a node's quotes as HTML — we rely on that for the literal
// <br/> the disclosure labels inject — so a '<' or '>' coming from the data (the
// "<dynamic>" effect marker, a generic type parameter) would be parsed as a (dropped)
// HTML tag, silently blanking part of the label: a confidently-wrong, silent render of
// exactly the dynamic-publish blind spot the honesty channel exists to surface. Escape
// the five HTML-significant characters so data stays literal while our own <br/> stays
// markup. Applied to data text only, never to the markup we compose around it.
func mermaidText(s string) string {
	return htmlEscaper.Replace(s)
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
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
	return text
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
