package graph

import (
	"strings"
	"testing"
)

// TestEBCTierNote pins the §21.A count enrichment: the note splits the
// ExternalBoundaryCall set by tier (fixed order, effect-bearing then trivial then
// unclassified), and is empty when no EBC is present so a non-EBC count reads clean.
func TestEBCTierNote(t *testing.T) {
	spots := []BlindSpot{
		{Kind: "reflect", Site: "p.F"},
		{Kind: "ExternalBoundaryCall", Site: "p.A", Severity: "effect-bearing"},
		{Kind: "ExternalBoundaryCall", Site: "p.B", Severity: "trivial"},
		{Kind: "ExternalBoundaryCall", Site: "p.C", Severity: "trivial"},
		{Kind: "ExternalBoundaryCall", Site: "p.D"}, // pre-tier graph → unclassified
	}
	if got, want := EBCTierNote(spots), " (1 effect-bearing, 2 trivial, 1 unclassified external)"; got != want {
		t.Errorf("EBCTierNote = %q, want %q", got, want)
	}
	if got := EBCTierNote([]BlindSpot{{Kind: "reflect"}, {Kind: "HighFanOut"}}); got != "" {
		t.Errorf("a set with no ExternalBoundaryCall must yield no note, got %q", got)
	}
	if got := EBCTierNote(nil); got != "" {
		t.Errorf("empty set must yield no note, got %q", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	const j = `{"nodes":[],"edges":[],"blind_spots":[],"surprise":1}`
	if _, err := Load(strings.NewReader(j)); err == nil {
		t.Fatal("expected an error for an unknown field, got nil")
	}
}

// TestLoadRejectsTrailingData pins M-15: a trusted graph file is exactly one JSON
// value. A second concatenated document (or any garbage after the object) means
// the input is not the single graph it claims to be, so Load must refuse rather
// than silently gate on only the first value.
func TestLoadRejectsTrailingData(t *testing.T) {
	cases := map[string]string{
		"concatenated graphs":    `{"nodes":[],"edges":[],"blind_spots":[]}{"nodes":[],"edges":[],"blind_spots":[]}`,
		"trailing garbage":       `{"nodes":[],"edges":[],"blind_spots":[]} oops`,
		"trailing array":         `{"nodes":[],"edges":[],"blind_spots":[]}[1,2,3]`,
		"trailing close brace":   `{"nodes":[],"edges":[],"blind_spots":[]}}`,
		"trailing close bracket": `{"nodes":[],"edges":[],"blind_spots":[]}]`,
	}
	for name, j := range cases {
		if _, err := Load(strings.NewReader(j)); err == nil {
			t.Errorf("%s: expected an error for trailing data, got nil", name)
		}
	}
	// A single well-formed graph (optionally with trailing whitespace/newline)
	// must still load cleanly — the guard rejects trailing *values*, not whitespace.
	if _, err := Load(strings.NewReader(`{"nodes":[],"edges":[],"blind_spots":[]}` + "\n")); err != nil {
		t.Errorf("single graph with trailing newline should load, got %v", err)
	}
}

// TestLoadRejectsInconsistentBoundaryMarking pins the H-5 decoder invariant: an
// edge's boundary-ness must agree between the To-prefix (IsBoundary(), what the
// reachability walk keys on) and the Boundary field. A boundary: To with an
// empty Boundary field — or a Boundary field on a non-boundary To — lets the two
// predicates disagree downstream and mask a real reachable violation, so Load
// must fail closed.
func TestLoadRejectsInconsistentBoundaryMarking(t *testing.T) {
	cases := map[string]string{
		"boundary To, empty field":   `{"nodes":[{"fqn":"svc.Purge","sig":"func()"}],"edges":[{"from":"svc.Purge","to":"boundary:db DELETE ledger"}],"blind_spots":[]}`,
		"non-boundary To, field set": `{"nodes":[{"fqn":"svc.A","sig":"func()"}],"edges":[{"from":"svc.A","to":"svc.B","boundary":"outbound-sync"}],"blind_spots":[]}`,
	}
	for name, j := range cases {
		if _, err := Load(strings.NewReader(j)); err == nil {
			t.Errorf("%s: expected an error for inconsistent boundary marking, got nil", name)
		}
	}
	// A well-formed boundary edge (prefix + field agree) must still load.
	const ok = `{"nodes":[{"fqn":"svc.Purge","sig":"func()"}],"edges":[{"from":"svc.Purge","to":"boundary:db DELETE ledger","boundary":"outbound-sync"}],"blind_spots":[]}`
	if _, err := Load(strings.NewReader(ok)); err != nil {
		t.Errorf("consistent boundary edge should load, got %v", err)
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
	// A caveat must surface even when the substrate is unrecorded: a trust disclosure
	// (reclaim, committed-corpus code-identity) the verdict leaned on may not be
	// silently dropped just because the call-graph algo was not recorded.
	if got := ProvenanceLine("", []string{"some trust disclosure"}); !strings.Contains(got, "unrecorded") || !strings.Contains(got, "some trust disclosure") {
		t.Errorf("an unrecorded substrate must still disclose its caveats, got %q", got)
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
	const j = `{"nodes":[{"fqn":"a","sig":"f"}],"edges":[{"from":"a","to":"boundary:db SELECT users","boundary":"outbound-sync"}],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rc := g.ReclaimCaveat(); rc != "" {
		t.Errorf("ReclaimCaveat on a base graph = %q, want empty", rc)
	}
}

// The SQL label fold tags a BOUNDARY edge (it recovers a verb, not an edge), so it
// must surface under SQLFoldCaveat and NOT under ReclaimCaveat — the two reclaimer
// KINDS stay independently auditable on the substrate line (plan §3, L3).
func TestSQLFoldCaveatSeparateFromEdgeReclaim(t *testing.T) {
	const j = `{"algo":"vta","nodes":[{"fqn":"a","sig":"f"}],"edges":[` +
		`{"from":"a","to":"svc.B","via":"strict-server"},` +
		`{"from":"a","to":"boundary:db DELETE","boundary":"outbound-sync","via":"sql-constfold"}],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rc := g.ReclaimCaveat()
	if !strings.Contains(rc, "1 via strict-server") || strings.Contains(rc, "sql-constfold") {
		t.Errorf("ReclaimCaveat must count only the edge reclaimer, got %q", rc)
	}
	sc := g.SQLFoldCaveat()
	if !strings.Contains(sc, "sql-fold-informed") || !strings.Contains(sc, "1 DB effect") {
		t.Errorf("SQLFoldCaveat must disclose the folded verb count, got %q", sc)
	}
	// Both ride the substrate line, distinctly.
	line := ProvenanceLine(g.Algo, []string{rc, sc})
	if !strings.Contains(line, "reclaim-informed") || !strings.Contains(line, "sql-fold-informed") {
		t.Errorf("provenance line must carry both disclosures, got %q", line)
	}
}

// H-10 wording bug: a `topic-constfold` fold recovers a BUS TOPIC, not a DB verb,
// so the caveat must describe it as a bus topic — the un-split count called every
// via-tagged boundary edge a "DB effect verb". A graph carrying BOTH folds must
// disclose each kind on its own, so a reader can tell what was actually recovered.
func TestSQLFoldCaveatSplitsByViaKind(t *testing.T) {
	const j = `{"algo":"vta","nodes":[{"fqn":"a","sig":"f"}],"edges":[` +
		`{"from":"a","to":"boundary:db INSERT users","boundary":"outbound-sync","via":"sql-constfold"},` +
		`{"from":"a","to":"boundary:bus PUBLISH loan.approved","boundary":"outbound-sync","via":"topic-constfold"}],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sc := g.SQLFoldCaveat()
	if !strings.Contains(sc, "1 DB effect verb") {
		t.Errorf("caveat must count the DB verb fold: %q", sc)
	}
	if !strings.Contains(sc, "1 bus topic") {
		t.Errorf("caveat must describe the topic fold as a bus topic, not a DB verb: %q", sc)
	}
}

// A folded label with no edge reclaimer leaves ReclaimCaveat silent — only the
// SQL disclosure fires.
func TestSQLFoldCaveatAloneLeavesReclaimSilent(t *testing.T) {
	const j = `{"nodes":[{"fqn":"a","sig":"f"}],"edges":[{"from":"a","to":"boundary:db INSERT users","boundary":"outbound-sync","via":"sql-constfold"}],"blind_spots":[]}`
	g, err := Load(strings.NewReader(j))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rc := g.ReclaimCaveat(); rc != "" {
		t.Errorf("ReclaimCaveat must be silent when only a label fold is present, got %q", rc)
	}
	if sc := g.SQLFoldCaveat(); !strings.Contains(sc, "sql-fold-informed") {
		t.Errorf("SQLFoldCaveat = %q, want the fold disclosure", sc)
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
