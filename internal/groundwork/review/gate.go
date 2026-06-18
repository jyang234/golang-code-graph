package review

import (
	"fmt"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/impeach"
)

// GateResult is the pre-flight verify verdict — the fail-closed sibling of the
// review artifact. Where Review explains a change to a human, Gate decides
// whether it may merge: it blocks on any newly-introduced invariant violation, on
// a touched package outside the declared scope (scope creep), or on a breaking
// inter-service contract change. Like Review it is a pure function of its inputs,
// carrying a reproducibility digest.
type GateResult struct {
	Service          string           `json:"service"`
	Pass             bool             `json:"pass"`
	NewViolations    []Violation      `json:"new_violations,omitempty"`
	ScopeEscapes     []string         `json:"scope_escapes,omitempty"`
	BreakingContract []ContractChange `json:"breaking_contract,omitempty"`
	NewBlindSpots    []BlindSpotDelta `json:"new_blind_spots,omitempty"`
	NewWriteTargets  []string         `json:"new_write_targets,omitempty"`
	// StandingCautions are the steady-state "the graph cannot prove this"
	// disclosures that hold identically on base and branch — the io_budget-
	// unenforceable case being the load-bearing one. They are NON-BLOCKING (they
	// never set Pass=false); they exist so the gate the agent actually converges
	// against stops being silent about a standing budget the diff suppresses
	// (R1). This is the only GateResult field that is not a reason the gate failed.
	StandingCautions []Violation `json:"standing_cautions,omitempty"`
	// ImpeachmentBreaches are the behaviorally-confirmed impeachment gate-blockers
	// (a committed-corpus VIOLATED, or a require_proof proof downgraded to
	// CANT-PROVE) supplied via WithImpeachment. They are ALWAYS disclosed here
	// (observe-first); they set Pass=false only when the policy opts in
	// (impeachment_gate.gate, §9/§10). Empty — and absent from the digest — for the
	// default static-only gate, so an existing verify is byte-identical.
	ImpeachmentBreaches []impeach.GateFinding `json:"impeachment_breaches,omitempty"`
	// Algo and Caveats record the call-graph substrate the judged branch graph
	// was built on — provenance baked into the gate digest so the verdict
	// self-certifies which algorithm produced it (R3). Caveats also carries the
	// committed-corpus code-identity disclosure when a behavioral impeachment is
	// present (committedCorpusIdentityCaveat) — a trust assumption on the same line,
	// not a call-graph property.
	Algo    string   `json:"algo,omitempty"`
	Caveats []string `json:"caveats,omitempty"`
	Digest  string   `json:"digest"`
}

// gateConfig holds the optional behavioral input a caller threads into the
// otherwise base/branch-static gate. The zero value is the pure static gate.
type gateConfig struct {
	impeachment []impeach.GateFinding
}

// GateOption configures an optional input to Gate. The default (no option) is the
// pure static base/branch gate, byte-identical to before behavioral integration.
type GateOption func(*gateConfig)

// WithImpeachment supplies behaviorally-confirmed impeachment gate-blockers —
// impeach.Resolution.GateBlockers computed over a COMMITTED corpus (the impeach
// layer already fences a live corpus out, §13 crack #2, so review never gates on
// run-varying traffic). They are disclosed in every gate result; they block only
// when the policy opts in via impeachment_gate.gate (§9/§10 observe-first).
func WithImpeachment(blockers []impeach.GateFinding) GateOption {
	return func(c *gateConfig) { c.impeachment = blockers }
}

// committedCorpusIdentityCaveat discloses the one rung of the impeachment downgrade
// ladder that a committed (stampless) corpus clears by INJECTION rather than by an
// independent check. On the `verify --corpus` gating path the committed golden
// carries no trace stamp, so committedImpeachmentBlockers injects the gated graph's
// OWN stamp as the trace identity (§14-E "the committed corpus takes the gated
// SHA"); the code-identity rung (§4 rung 2) then compares the graph's stamp against
// itself. That rung therefore confirms only that the graph IS stamped — never that
// the corpus was captured from the gated code. The version-skew protection it exists
// for is thus an operational assumption on this path (corpus freshness), not a
// mechanical check: a stale golden asserting an effect the current code no longer
// emits would yield a false BLOCK the ladder cannot catch (errs safe — never a false
// PASS). Disclosed so a reviewer reads freshness as the assumption it is — the
// behavioral analog of R11's "regenerate from current source" trust-anchor — beside
// the reclaim/substrate provenance caveats it mirrors.
const committedCorpusIdentityCaveat = "impeachment gated on a committed corpus; code-identity is asserted by the gated stamp, not verified against a trace stamp — ensure the corpus is re-captured for this commit"

// Gate runs the pre-flight checks. scope is the set of package-path prefixes the
// change is declared to be confined to; an empty scope disables the scope-creep
// check (only new violations and breaking contract gate).
func Gate(p *policy.Policy, base, branch *graph.Graph, scope []string, opts ...GateOption) GateResult {
	var cfg gateConfig
	for _, o := range opts {
		o(&cfg)
	}
	baseIx, branchIx := graph.NewIndex(base), graph.NewIndex(branch)
	d := diffGraphs(base, branch)

	newViolations, _, standingCautions, _, _ := newFindings(p, baseIx, branchIx)

	var breaking []ContractChange
	for _, c := range contractChanges(baseIx, branchIx) {
		if c.Breaking {
			breaking = append(breaking, c)
		}
	}

	escapes := scopeEscapes(d, scope)

	// New blind spots gate only when the policy says so (blind_spot_ratchet.gate);
	// otherwise they are review-artifact information, not a merge blocker — so
	// every BLOCKING field a GateResult lists is a reason it failed (StandingCautions
	// is the one disclosure-only exception).
	var blindSpots []BlindSpotDelta
	if p.GatesBlindSpots() {
		blindSpots = newBlindSpots(p, base, branch)
	}

	// Same policy-controlled gating for the effect ratchet: new write targets
	// block only when effect_ratchet.gate is set.
	var writeTargets []string
	if p.GatesEffects() {
		writeTargets = newWriteTargets(p, base, branch)
	}

	// When a behavioral impeachment is present it cleared the code-identity rung by
	// INJECTION, not by an independent check (the committed corpus is stamped with the
	// gated graph's own stamp, §14-E) — so disclose that corpus freshness is an
	// operational assumption on this path. Emitted only ALONGSIDE a present impeachment
	// (the finding it qualifies), so the default static gate — and WithImpeachment(nil)
	// — stays byte-identical, carrying no stray caveat into the digest.
	caveats := provenanceCaveats(p.Substrate, base, branch)
	if len(cfg.impeachment) > 0 {
		caveats = append(caveats, committedCorpusIdentityCaveat)
	}

	g := GateResult{
		Service:             p.Service,
		NewViolations:       newViolations,
		ScopeEscapes:        escapes,
		BreakingContract:    breaking,
		NewBlindSpots:       blindSpots,
		NewWriteTargets:     writeTargets,
		StandingCautions:    standingCautions,
		ImpeachmentBreaches: cfg.impeachment,
		Algo:                branch.Algo,
		Caveats:             caveats,
	}
	// Impeachment breaches are ALWAYS disclosed (above) but gate only on the
	// opt-in (§9/§10 observe-first): a breach DISCLOSES from day one and BLOCKS only
	// once impeachment_gate.gate is ratified. Mirrors the blind-spot/effect ratchets.
	blockingImpeach := 0
	if p.GatesImpeachment() {
		blockingImpeach = len(cfg.impeachment)
	}
	// StandingCautions are deliberately absent from this conjunction: they
	// disclose, they do not gate.
	g.Pass = len(newViolations) == 0 && len(escapes) == 0 && len(breaking) == 0 && len(blindSpots) == 0 && len(writeTargets) == 0 && blockingImpeach == 0
	g.Digest = gateDigest(g)
	return g
}

// scopeEscapes returns the touched packages that fall outside the declared scope.
// touchedPackages already includes both node-change and edge-endpoint packages, so
// a change that edits package A but wires a new edge into package B is caught even
// when B gained no node.
func scopeEscapes(d graphDelta, scope []string) []string {
	if len(scope) == 0 {
		return nil
	}
	var out []string
	for _, pkg := range d.touchedPackages() { // already sorted
		if !withinScope(pkg, scope) {
			out = append(out, pkg)
		}
	}
	return out
}

func withinScope(pkg string, scope []string) bool {
	for _, s := range scope {
		// Identifier-boundary match (not bare HasPrefix): a scope of
		// "...internal/app" admits its sub-packages ("...internal/app/sub") but
		// must NOT admit the sibling "...internal/application", which a bare
		// prefix would silently let escape the gate.
		if policy.MatchPrefix(pkg, s) {
			return true
		}
	}
	return false
}

func gateDigest(g GateResult) string {
	g.Digest = ""
	return canonicalDigest(g)
}

// Render is the human-facing gate report.
func (g GateResult) Render() string {
	var b strings.Builder
	verdict := "PASS"
	if !g.Pass {
		verdict = "BLOCK"
	}
	fmt.Fprintf(&b, "# Pre-flight gate — %s\n", verdict)
	fmt.Fprintf(&b, "digest %s · recompute to verify (deterministic; not author-editable)\n", short(g.Digest))
	b.WriteString(renderProvenance(g.Algo, g.Caveats))

	if g.Pass {
		b.WriteString("\nNo new violations, no scope escapes, no breaking contract changes.\n")
		// A passing gate still surfaces standing cautions: this is the gate path
		// R1 found silent — a steady-state unenforceable budget must be visible
		// where the agent actually converges, not only in the fitness lens.
		if s := renderStandingCautions(g.StandingCautions); s != "" {
			b.WriteString("\n")
			b.WriteString(s)
		}
		// Impeachment breaches disclose even on PASS (observe-first): when the
		// opt-in is off they do not block, but the behavioral counterexample is
		// still shown where the agent converges, never silently dropped.
		if s := renderImpeachmentBreaches(g.ImpeachmentBreaches); s != "" {
			b.WriteString("\n")
			b.WriteString(s)
		}
		return b.String()
	}
	b.WriteString("\n")
	if len(g.NewViolations) > 0 {
		fmt.Fprintf(&b, "⛔ %d new invariant violation(s)\n", len(g.NewViolations))
		for _, v := range g.NewViolations {
			fmt.Fprintf(&b, "- %s — %s\n", v.Rule, v.Summary)
			if v.From != "" {
				fmt.Fprintf(&b, "  - %s\n", edge(v))
			}
		}
	}
	if len(g.ScopeEscapes) > 0 {
		fmt.Fprintf(&b, "🚧 %d package(s) outside the declared scope\n", len(g.ScopeEscapes))
		for _, p := range g.ScopeEscapes {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	if len(g.BreakingContract) > 0 {
		fmt.Fprintf(&b, "🔌 %d breaking contract change(s)\n", len(g.BreakingContract))
		for _, c := range g.BreakingContract {
			fmt.Fprintf(&b, "- %s %s %s\n", c.Op, c.Surface, c.Name)
		}
	}
	if len(g.NewBlindSpots) > 0 {
		fmt.Fprintf(&b, "🕳️  %d new blind spot(s) — gated by blind_spot_ratchet; allow-list with a reason or remove the dynamic construct\n", len(g.NewBlindSpots))
		for _, s := range g.NewBlindSpots {
			fmt.Fprintf(&b, "- %s %s\n", s.Kind, s.Site)
		}
	}
	if len(g.NewWriteTargets) > 0 {
		fmt.Fprintf(&b, "🧾 %d new external write target(s) — gated by effect_ratchet; allow-list with a reason or drop the write\n", len(g.NewWriteTargets))
		for _, t := range g.NewWriteTargets {
			fmt.Fprintf(&b, "- %s\n", t)
		}
	}
	if s := renderImpeachmentBreaches(g.ImpeachmentBreaches); s != "" {
		b.WriteString(s)
	}
	if s := renderStandingCautions(g.StandingCautions); s != "" {
		b.WriteString(s)
	}
	return b.String()
}

// renderImpeachmentBreaches discloses behaviorally-confirmed impeachments — a
// VIOLATED (observed must_not_reach breach) or a require_proof proof downgraded to
// CANT-PROVE. Disclosed always; blocking only when impeachment_gate.gate is set
// (the verdict line at the top conveys whether it did). "" when there are none.
func renderImpeachmentBreaches(bs []impeach.GateFinding) string {
	if len(bs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔭 %d behavioral impeachment(s) — gated by impeachment_gate (disclosed always; blocks only when ratified)\n", len(bs))
	for _, f := range bs {
		fmt.Fprintf(&b, "- %s: %s on flow %q reaching %q — %s\n", f.Verdict, f.Rule, f.Flow, f.Effect, f.Reason)
	}
	return b.String()
}

// Marshal renders the gate result as canonical JSON.
func (g GateResult) Marshal() ([]byte, error) { return canonjson.Marshal(g) }
