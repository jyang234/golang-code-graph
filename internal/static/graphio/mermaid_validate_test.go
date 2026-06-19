package graphio

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
)

// validateMermaid asserts the structural validity of a rendered flowchart — the
// invariants this package controls and that Mermaid's parser cares about — so output
// correctness is checked MECHANICALLY rather than eyeballed (CLAUDE.md: verify
// mechanically). It models the exact failure mode of the "<dynamic>" bug: in Mermaid's
// HTML-label mode, a '<' or '>' in a label that is not part of an allowed <br/> tag is
// parsed as a (dropped) tag, silently blanking the label. It also pins balanced label
// quotes and that every :::class is defined. It accepts an optional ```mermaid fence.
func validateMermaid(diagram string) error {
	body := strings.TrimPrefix(diagram, "```mermaid\n")
	body = strings.TrimSuffix(body, "```\n")
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "flowchart LR" {
		got := ""
		if len(lines) > 0 {
			got = lines[0]
		}
		return fmt.Errorf("first line must be 'flowchart LR', got %q", got)
	}
	if len(lines) == 1 {
		return fmt.Errorf("a flowchart with no nodes is not valid Mermaid")
	}

	defined := map[string]bool{}
	type ref struct{ class, line string }
	var refs []ref
	classRef := regexp.MustCompile(`:::([A-Za-z0-9_]+)`)

	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "classDef ") {
			if f := strings.Fields(trimmed); len(f) >= 2 {
				defined[f[1]] = true
			}
		}
		for _, m := range classRef.FindAllStringSubmatch(ln, -1) {
			refs = append(refs, ref{m[1], ln})
		}
		// %% comment text is free-form; only validate quoted-label lines.
		if strings.HasPrefix(trimmed, "%%") || !strings.Contains(ln, `"`) {
			continue
		}
		if c := strings.Count(ln, `"`); c != 2 {
			return fmt.Errorf("line %d has %d quote chars (want exactly 2 — an unescaped quote leaked into a label): %q", i+1, c, ln)
		}
		lo := strings.IndexByte(ln, '"')
		hi := strings.LastIndexByte(ln, '"')
		label := ln[lo+1 : hi]
		stripped := strings.ReplaceAll(label, "<br/>", "")
		stripped = strings.ReplaceAll(stripped, "<br>", "")
		if strings.ContainsAny(stripped, "<>") {
			return fmt.Errorf("line %d label %q carries a raw <,> outside an allowed <br/> tag; "+
				"Mermaid's HTML-label mode would drop it (escape via mermaidText): %q", i+1, label, ln)
		}
	}

	// Dialect floor: every content line must be one of the constructs we deliberately
	// emit (see the Mermaid-compatibility note in docs/guides/adopting-flowmap.md). A
	// line we don't recognize means a new Mermaid feature crept in — fail here so its
	// cross-host compatibility (notably older GitLab-pinned mermaid) is reviewed before
	// it ships, rather than silently breaking a reviewer's rendered view.
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		switch {
		case i == 0, trimmed == "":
		case strings.HasPrefix(trimmed, "%%"),
			strings.HasPrefix(trimmed, "classDef "),
			strings.HasPrefix(trimmed, "linkStyle "),
			strings.HasPrefix(trimmed, "subgraph "),
			trimmed == "direction LR",
			trimmed == "end":
		case strings.Contains(ln, "-->"), strings.Contains(ln, "==>"), strings.Contains(ln, ".->"):
			// an edge (optionally with an inline node declaration / label)
		case strings.Contains(ln, `"`):
			// a node declaration; its label was checked above
		default:
			return fmt.Errorf("line %d is not a recognized Mermaid construct (dialect floor — "+
				"vet cross-host compatibility before adding): %q", i+1, ln)
		}
	}
	for _, r := range refs {
		if !defined[r.class] {
			return fmt.Errorf("class %q is referenced but no classDef defines it: %q", r.class, r.line)
		}
	}
	return nil
}

func assertValidMermaid(t *testing.T, diagram string) {
	t.Helper()
	if err := validateMermaid(diagram); err != nil {
		t.Errorf("invalid Mermaid output: %v\n--- full ---\n%s", err, diagram)
	}
}

// TestMermaidValidatorCatchesRawAngleBrackets proves the validator would have caught
// the original "<dynamic>" bug: an unescaped angle-bracket label must be rejected,
// while the legitimate <br/> markup we inject is accepted.
func TestMermaidValidatorCatchesRawAngleBrackets(t *testing.T) {
	bad := "flowchart LR\n    n0{{\"bus PUBLISH <dynamic>\"}}:::bus\n    classDef bus fill:#efe\n"
	if validateMermaid(bad) == nil {
		t.Error("validator must reject a raw <dynamic> label")
	}
	good := "flowchart LR\n    n0([\"⊥ reflect<br/>blind spot\"]):::blind\n    classDef blind fill:#fde\n"
	if err := validateMermaid(good); err != nil {
		t.Errorf("validator must accept the legitimate <br/> markup, got: %v", err)
	}
	undefclass := "flowchart LR\n    n0[\"x\"]:::ghost\n"
	if validateMermaid(undefclass) == nil {
		t.Error("validator must reject a :::class with no classDef")
	}
}

// TestMermaidValidityAdversarial renders graphs whose labels carry the characters
// most likely to break Mermaid — angle brackets, quotes, generics, pipes, unicode —
// and asserts every renderer emits valid output.
func TestMermaidValidityAdversarial(t *testing.T) {
	g := &Graph{
		Algo: "rta",
		Nodes: []Node{
			{FQN: "(*example.com/p.Cache[string]).Get", Tier: 1, Fallible: true},
			{FQN: "example.com/p.run", Tier: 1},
			{FQN: "example.com/p.dynamic", Tier: 2},
		},
		Edges: []Edge{
			{From: "example.com/p.run", To: "(*example.com/p.Cache[string]).Get", Tier: 2},
			{From: "example.com/p.run", To: `boundary:db EXEC INSERT "x" <- y | z`, Tier: 1, Boundary: "outbound-sync", Via: "sql-constfold"},
			{From: "example.com/p.dynamic", To: "boundary:bus PUBLISH <dynamic>", Tier: 1, Boundary: "outbound-async"},
		},
		BlindSpots: []blindspots.BlindSpot{{Kind: blindspots.Reflect, Site: "example.com/p.run"}},
		Frontier: &FrontierSection{Markers: []frontier.Marker{
			{Kind: "dynamic-effect", Bin: frontier.BinA, Site: "bus PUBLISH <dynamic>", Owner: "example.com/p.dynamic"},
		}},
	}
	assertValidMermaid(t, g.Mermaid(MermaidOptions{MaxTier: 2}))
	assertValidMermaid(t, g.Mermaid(MermaidOptions{MaxTier: 0}))

	// Rooted and diff renderers over the same adversarial data.
	g.Entrypoints = []Entrypoint{{Kind: "http", Name: "POST /run", Fn: "example.com/p.run"}}
	if out, ok := g.MermaidRootedAt("POST /run", MermaidOptions{MaxTier: 2}); ok {
		assertValidMermaid(t, out)
	}
	assertValidMermaid(t, MermaidDiff(&Graph{Algo: "rta"}, g, MermaidOptions{MaxTier: 2}))

	// The empty/degenerate graph must still be valid (placeholder node).
	assertValidMermaid(t, (&Graph{Algo: "rta"}).Mermaid(MermaidOptions{MaxTier: 2}))
}
