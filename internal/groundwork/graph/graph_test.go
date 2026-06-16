package graph

import (
	"strings"
	"testing"
)

func TestLoadRejectsUnknownFields(t *testing.T) {
	const j = `{"nodes":[],"edges":[],"blind_spots":[],"surprise":1}`
	if _, err := Load(strings.NewReader(j)); err == nil {
		t.Fatal("expected an error for an unknown field, got nil")
	}
}

// A graph carrying flowmap's recorded algo/caveats must round-trip (the schema
// must accept the provenance keys it now emits), and the substrate line must
// echo them. An empty algo reads as "unrecorded", never as a substrate (R3).
func TestProvenanceLineAndRoundTrip(t *testing.T) {
	const j = `{"algo":"vta","caveats":["vta refined over rta from 3 discovered root(s)"],"nodes":[],"edges":[],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("provenance keys must round-trip, got %v", err)
	}
	if g.Algo != "vta" || len(g.Caveats) != 1 {
		t.Fatalf("algo=%q caveats=%v, want vta + 1 caveat", g.Algo, g.Caveats)
	}
	line := ProvenanceLine(g.Algo, g.Caveats)
	if !strings.Contains(line, "substrate: vta") || !strings.Contains(line, "refined over rta") {
		t.Errorf("provenance line = %q", line)
	}
	if got := ProvenanceLine("", nil); !strings.Contains(got, "unrecorded") {
		t.Errorf("empty algo must read as unrecorded, got %q", got)
	}
}

// A graph carrying flowmap's producing-build version (the `tool` header, R11)
// must round-trip — the decoder rejects unknown fields, so a field flowmap now
// emits has to be taught here — and ToolMismatchCaveat must disclose a base/branch
// producer skew while staying silent on a match or an unrecorded side.
func TestToolFieldRoundTripAndMismatchCaveat(t *testing.T) {
	const j = `{"tool":"flowmap-1.2.3","algo":"vta","nodes":[],"edges":[],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("the producer (tool) field must round-trip, got %v", err)
	}
	if g.Tool != "flowmap-1.2.3" {
		t.Fatalf("tool=%q, want flowmap-1.2.3", g.Tool)
	}
	if got := ToolMismatchCaveat("a", "b"); !strings.Contains(got, "producer mismatch") || !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Errorf("a mismatch must name both producers; got %q", got)
	}
	for _, c := range []struct{ base, branch string }{
		{"a", "a"}, // agree (dogfood: one pinned binary)
		{"", "b"},  // base unrecorded
		{"a", ""},  // branch unrecorded
		{"", ""},   // both unrecorded
	} {
		if got := ToolMismatchCaveat(c.base, c.branch); got != "" {
			t.Errorf("ToolMismatchCaveat(%q,%q) = %q, want silent", c.base, c.branch, got)
		}
	}
}

// The base↔branch algo mismatch is the sibling of the tool and substrate caveats,
// and like them is a unit-testable helper (not inline at the call site): it must
// name both algorithms when they differ and stay silent when either is unrecorded
// or the two agree. The wording is pinned because review's substrate-disclosure
// tests match the "substrate differs" phrase.
func TestAlgoMismatchCaveat(t *testing.T) {
	if got := AlgoMismatchCaveat("rta", "vta"); !strings.Contains(got, "rta") || !strings.Contains(got, "vta") || !strings.Contains(got, "substrate differs") {
		t.Errorf("a mismatch must name both algos and say 'substrate differs'; got %q", got)
	}
	for _, c := range []struct{ base, branch string }{
		{"vta", "vta"}, // agree
		{"", "rta"},    // base unrecorded
		{"vta", ""},    // branch unrecorded
		{"", ""},       // both unrecorded
	} {
		if got := AlgoMismatchCaveat(c.base, c.branch); got != "" {
			t.Errorf("AlgoMismatchCaveat(%q,%q) = %q, want silent", c.base, c.branch, got)
		}
	}
}

// A graph from `flowmap graph --reclaim` carries a per-edge `via` provenance tag
// (R9). groundwork must CONSUME it — the decoder rejected it before, so every
// command died on a reclaimed graph — and ReclaimCaveat must disclose it so a
// verdict computed over the reclaimed substrate is auditable as reclaim-informed.
func TestReclaimEdgeRoundTripAndCaveat(t *testing.T) {
	const j = `{"algo":"vta","nodes":[{"fqn":"svc/api.Wrapper.Create","sig":"f"},{"fqn":"svc/api.Wrapper.Create$1","sig":"f"}],"edges":[{"from":"svc/api.Wrapper.Create","to":"svc/api.Wrapper.Create$1","via":"strict-server"}],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("a reclaimed graph (via field) must round-trip, got %v", err)
	}
	if len(g.Edges) != 1 || g.Edges[0].Via != "strict-server" {
		t.Fatalf("via did not round-trip: edges=%+v", g.Edges)
	}
	rc := g.ReclaimCaveat()
	if !strings.Contains(rc, "reclaim-informed") || !strings.Contains(rc, "1 via strict-server") {
		t.Errorf("ReclaimCaveat = %q, want it to name the reclaimer and count", rc)
	}
	// The disclosure must ride the substrate line every verdict surface echoes.
	line := ProvenanceLine(g.Algo, []string{rc})
	if !strings.Contains(line, "substrate: vta") || !strings.Contains(line, "reclaim-informed") {
		t.Errorf("provenance line = %q, want substrate + reclaim disclosure", line)
	}
}

// A base (no --reclaim) graph carries no via, so ReclaimCaveat is silent — the
// committed, byte-identical default graph must disclose nothing.
func TestReclaimCaveatSilentOnBaseGraph(t *testing.T) {
	const j = `{"nodes":[{"fqn":"a","sig":"f"}],"edges":[{"from":"a","to":"boundary:db SELECT users"}],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rc := g.ReclaimCaveat(); rc != "" {
		t.Errorf("ReclaimCaveat on a base graph = %q, want empty", rc)
	}
}

// The substrate guard flags a policy-vs-graph algorithm mismatch (§9): a policy
// proposed on one algorithm checked against a graph built on another can surface
// spurious reachability findings (precision differs). It must stay silent when
// either side is unrecorded or the two agree, and name both algorithms when they
// differ so the reader can act (rebuild the graph, or re-init the policy).
func TestSubstrateMismatchCaveat(t *testing.T) {
	if got := SubstrateMismatchCaveat("vta", "rta"); !strings.Contains(got, "vta") || !strings.Contains(got, "rta") || !strings.Contains(got, "--algo vta") {
		t.Errorf("mismatch must name both algos and the remedy; got %q", got)
	}
	for _, c := range []struct{ pol, gph string }{
		{"vta", "vta"}, // agree
		{"", "rta"},    // policy unrecorded
		{"vta", ""},    // graph unrecorded
		{"", ""},       // both unrecorded
	} {
		if got := SubstrateMismatchCaveat(c.pol, c.gph); got != "" {
			t.Errorf("SubstrateMismatchCaveat(%q,%q) = %q, want silent", c.pol, c.gph, got)
		}
	}
}

func TestLoadRequiresNodes(t *testing.T) {
	const j = `{"edges":[],"blind_spots":[]}`
	if _, err := Load(strings.NewReader(j)); err == nil {
		t.Fatal("expected an error for a graph with no nodes key, got nil")
	}
}

func TestLoadEmptyGraph(t *testing.T) {
	const j = `{"nodes":[],"edges":[],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(g.Nodes) != 0 {
		t.Fatalf("want 0 nodes, got %d", len(g.Nodes))
	}
}

func TestEdgeClassification(t *testing.T) {
	cases := []struct {
		to       string
		boundary bool
		dynamic  bool
	}{
		{"example.com/svc/internal/app.Do", false, false},
		{"boundary:db INSERT users", true, false},
		{"boundary:bus PUBLISH user.created", true, false},
		{"boundary:bus PUBLISH <dynamic>", true, true},
	}
	for _, c := range cases {
		e := Edge{To: c.to}
		if got := e.IsBoundary(); got != c.boundary {
			t.Errorf("IsBoundary(%q)=%v, want %v", c.to, got, c.boundary)
		}
		if got := e.IsDynamic(); got != c.dynamic {
			t.Errorf("IsDynamic(%q)=%v, want %v", c.to, got, c.dynamic)
		}
	}
}
