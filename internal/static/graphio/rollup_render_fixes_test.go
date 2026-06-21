package graphio

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// hostileNoteGraph reproduces the C3 render-correctness bug (addendum item 1): a disclosed
// edge whose annotation note carries the Mermaid-flowchart-structural characters
// labelEscaper does NOT cover — a pipe and surrounding parens, exactly the Customer.io note
// ("…served by GET /debug/customerio/events…" with parens) that broke the cgate C3 render.
func hostileNoteGraph() *Graph {
	const serve = "(*ex.com/svc/server.S).Serve"
	return &Graph{
		Algo:  "rta",
		Nodes: []Node{{FQN: serve, Package: "ex.com/svc/server"}},
		BlindSpots: []blindspots.BlindSpot{
			{Kind: blindspots.ExternalBoundaryCall, Site: serve, Package: "github.com/customerio/go-customerio", Severity: blindspots.SeverityEffectBearing},
		},
		Annotations: []Annotation{
			{Site: serve, Kind: "ExternalBoundaryCall", Claim: "POSTs (served by GET /debug | events)"},
		},
	}
}

// TestRollupEdgeLabelEscapesStructuralChars pins the fix: a disclosed-edge annotation note
// with a pipe and parens must still render to STRUCTURALLY VALID Mermaid (quoting the edge
// label neutralizes the delimiters), in the plain, banded, and diff renders — the three
// paths that emit a note as an edge label. Before the fix the pipe terminated the label
// early and the parens broke the parse.
func TestRollupEdgeLabelEscapesStructuralChars(t *testing.T) {
	g := hostileNoteGraph()
	assertValidMermaid(t, g.RollupMermaid(RollupMermaidOptions{}))
	assertValidMermaid(t, g.RollupMermaid(RollupMermaidOptions{Bands: true}))
	// The diff render carries the same note on a kept disclosed edge.
	assertValidMermaid(t, RollupMermaidDiff(g, g, RollupMermaidOptions{}))

	// The note's text survives, not truncated at the pipe: the data pipe is carried as its
	// &#124; entity (not a bare pipe that would terminate the label early).
	out := g.RollupMermaid(RollupMermaidOptions{})
	if !strings.Contains(out, "&#124;") {
		t.Errorf("expected the data pipe to be carried as an entity, got:\n%s", out)
	}
	if strings.Contains(out, "GET /debug | events") {
		t.Errorf("a raw pipe leaked into the edge label (would terminate it early):\n%s", out)
	}
}

// TestRollupEdgeLabelHelper pins the quoting contract directly: the label is wrapped in
// quotes and the structural characters are escaped/neutralized, so a downstream pipe count
// stays balanced.
func TestRollupEdgeLabelHelper(t *testing.T) {
	got := rollupEdgeLabel("a|b(c)")
	if !strings.HasPrefix(got, `|"`) || !strings.HasSuffix(got, `"|`) {
		t.Errorf("edge label must be quoted, got %q", got)
	}
	// Exactly two structural pipes (the delimiters); the data pipe is HTML-escaped, so it
	// is not a third bare pipe that would unbalance the label.
	if n := strings.Count(got, "|"); n != 2 {
		t.Errorf("edge label has %d pipes, want 2 (data pipe must be escaped): %q", n, got)
	}
}

// TestRollupSubstrateLine pins the substrate disclosure (addendum item 2): the Graph-level
// render header carries the algo and reclaimer footprint so a viewer knows which build the
// rollup came from — a rollup's fidelity is build-flag-dependent. The bare PackageRollup
// render (no producing graph) carries no substrate line.
func TestRollupSubstrateLine(t *testing.T) {
	g := bandedSampleGraph()
	g.Algo = "vta"
	out := g.RollupMermaid(RollupMermaidOptions{})
	if !strings.Contains(out, "substrate") || !strings.Contains(out, "algo: vta") {
		t.Errorf("Graph-level rollup render must disclose the substrate (algo), got:\n%s", out)
	}
	if !strings.Contains(out, "reclaimer: off") {
		t.Errorf("a plain (un-reclaimed) build must disclose reclaimer: off, got:\n%s", out)
	}

	// A reclaimed build (a via-tagged edge present) flips the reclaimer footprint.
	g.Edges = append(g.Edges, Edge{From: "ex.com/svc/app.Run", To: "boundary:db DELETE x", Via: "sql-passthrough"})
	if recl := g.RollupMermaid(RollupMermaidOptions{}); !strings.Contains(recl, "reclaimer: on") {
		t.Errorf("a reclaimed build (via-tagged edge) must disclose reclaimer: on, got:\n%s", recl)
	}

	// The bare PackageRollup render has no graph, so no substrate line.
	if bare := g.RollupByPackage().Mermaid(RollupMermaidOptions{}); strings.Contains(bare, "substrate") {
		t.Errorf("the bare PackageRollup render must NOT carry a substrate line, got:\n%s", bare)
	}
}

// TestRollupSubstrateUnrecorded pins that a tool-stripped graph (no algo) renders the
// substrate as "unrecorded" rather than an empty/misleading "algo: " — matching the
// provenanceCaveats "unrecorded, not a mismatch" treatment.
func TestRollupSubstrateUnrecorded(t *testing.T) {
	g := bandedSampleGraph() // no Algo set
	if s := rollupSubstrate(g); !strings.Contains(s, "unrecorded") {
		t.Errorf("an algo-less graph must render substrate as unrecorded, got %q", s)
	}
}
