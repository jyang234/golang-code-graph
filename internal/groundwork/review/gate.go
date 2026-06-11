package review

import (
	"fmt"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
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
	Digest           string           `json:"digest"`
}

// Gate runs the pre-flight checks. scope is the set of package-path prefixes the
// change is declared to be confined to; an empty scope disables the scope-creep
// check (only new violations and breaking contract gate).
func Gate(p *policy.Policy, base, branch *graph.Graph, scope []string) GateResult {
	baseIx, branchIx := graph.NewIndex(base), graph.NewIndex(branch)
	d := diffGraphs(base, branch)

	newViolations, _ := newFindings(p, baseIx, branchIx)

	var breaking []ContractChange
	for _, c := range contractChanges(d, baseIx, branchIx) {
		if c.Breaking {
			breaking = append(breaking, c)
		}
	}

	escapes := scopeEscapes(d, scope)

	// New blind spots gate only when the policy says so (blind_spot_ratchet.gate);
	// otherwise they are review-artifact information, not a merge blocker — so
	// everything a GateResult lists is a reason it failed.
	var blindSpots []BlindSpotDelta
	if p.GatesBlindSpots() {
		blindSpots = newBlindSpots(p, base, branch)
	}

	g := GateResult{
		Service:          p.Service,
		NewViolations:    newViolations,
		ScopeEscapes:     escapes,
		BreakingContract: breaking,
		NewBlindSpots:    blindSpots,
	}
	g.Pass = len(newViolations) == 0 && len(escapes) == 0 && len(breaking) == 0 && len(blindSpots) == 0
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
		if pkg == s || strings.HasPrefix(pkg, s) {
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

	if g.Pass {
		b.WriteString("\nNo new violations, no scope escapes, no breaking contract changes.\n")
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
	return b.String()
}

// Marshal renders the gate result as canonical JSON.
func (g GateResult) Marshal() ([]byte, error) { return canonjson.Marshal(g) }
