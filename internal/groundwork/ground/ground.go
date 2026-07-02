// Package ground assembles the pre-edit grounding card (GX-5): everything an
// agent (or human) should know BEFORE touching a function — its identity and
// neighborhood, the external effects it can reach, the rules that demonstrably
// bind it, and the blind spots touching any claim on the card. Deterministic
// prevention is strictly cheaper than deterministic rejection: the same rules
// that gate the merge are surfaced at generation time, so the edit loop
// becomes ground → edit → verify with one rule set at both ends.
//
// The card never promises a guardrail that does not bind: binding rules are
// derived with the same matchers the checks use (fitness.MatchesAny, the
// graph-carried obligation and effect-order facts), and a card whose claims
// cross a blind spot says so.
package ground

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Card is the grounding evidence for one function. Pure function of
// (graph, policy, fqn); every list is sorted.
type Card struct {
	FQN         string            `json:"fqn"`
	Sig         string            `json:"sig,omitempty"`
	Tier        int               `json:"tier,omitempty"`
	Layer       string            `json:"layer,omitempty"` // the policy layer owning its package
	Callers     []string          `json:"callers,omitempty"`
	Callees     []string          `json:"callees,omitempty"`
	Entrypoints []string          `json:"entrypoints,omitempty"` // cover: what it is live behind
	Effects     []string          `json:"effects,omitempty"`     // reachable boundary effects
	Binding     []string          `json:"binding_rules,omitempty"`
	BlindSpots  []graph.BlindSpot `json:"blind_spots,omitempty"`
	// Annotations is the human/AI context attached to this card's blind spots
	// (keyed by Site/Kind). Disclosure only — it explains a blind spot the analysis
	// cannot close, it never changes a claim on the card.
	Annotations []graph.Annotation `json:"annotations,omitempty"`

	// CoverOverApprox marks the entrypoint cover as an upper bound: the backward
	// reach to those entrypoints passed through a HighFanOut dispatch seam, which
	// fans every caller onto every implementation, so the count is "≤", not "=".
	CoverOverApprox bool `json:"cover_over_approx,omitempty"`
}

// For assembles the card. The policy may be nil (graph-only grounding); the
// binding-rules section then carries only the graph-borne facts (obligations,
// effect order).
func For(ix *graph.Index, p *policy.Policy, fqn string) (Card, error) {
	node, ok := ix.Node(fqn)
	if !ok {
		return Card{}, fmt.Errorf("no function %q in graph", fqn)
	}
	c := Card{FQN: fqn, Sig: node.Sig, Tier: node.Tier}
	c.Callers = append([]string{}, ix.Callers(fqn)...)
	c.Callees = append([]string{}, ix.Callees(fqn)...)
	// reaching is fqn's reverse-reachable set (an O(V+E) BFS). The card needs it
	// twice — for the entrypoint cover here and the blind-spot frontier below — so
	// compute it once and feed both, rather than running the BFS twice per call on
	// the MCP serving path (ground is invoked before every edit).
	reaching := ix.Reaching(fqn)
	c.Entrypoints = ix.EntrypointCoverFrom(fqn, setutil.StringSet(reaching))

	cone := append([]string{fqn}, ix.Reachable(fqn)...)
	coneEffects := ix.Effects(cone...)
	effects := map[string]bool{}
	for _, e := range coneEffects {
		effects[e.To] = true
	}
	c.Effects = setutil.SortedKeys(effects)

	c.Binding = bindingRules(ix, p, fqn, &c)

	// Blind spots are gathered over the WHOLE traversed frontier — the backward
	// reach (who the entrypoint-cover claim crosses) as well as the forward cone
	// (what the effects claim crosses) — at parity with reach/triage. The earlier
	// forward-only scope left the cover claim, the line an agent reads first,
	// undefended against the HighFanOut dispatch that inflates it (F4). The
	// <dynamic> boundary effects in the cone are synthesized into blind spots too:
	// they are the frontier the effects claim stops being sound past, and they do
	// not appear in the graph's blind_spots[] manifest.
	traversed := append(append([]string{}, cone...), reaching...)
	seen := map[string]bool{}
	addBlind := func(b graph.BlindSpot) {
		// Detail is part of the identity: distinct DynamicEffect labels at one site
		// are distinct disclosures (parity with impact.addBlind).
		k := b.Kind + "\x00" + b.Site + "\x00" + b.Detail
		if !seen[k] {
			seen[k] = true
			c.BlindSpots = append(c.BlindSpots, b)
		}
	}
	for _, fn := range traversed {
		for _, b := range ix.BlindSpotsAt(fn) {
			addBlind(b)
		}
		for _, b := range ix.BlindSpotsAt(fitness.PkgOf(fn)) {
			addBlind(b)
		}
	}
	for _, e := range coneEffects {
		if e.IsDynamic() {
			addBlind(graph.BlindSpot{Kind: "DynamicEffect", Site: e.From, Detail: strings.TrimPrefix(e.To, "boundary:")})
		}
	}

	// The cover crosses a dispatch seam iff a HighFanOut sits on the backward
	// reach — then the entrypoint count is an over-approximation (F3).
	c.CoverOverApprox = ix.CrossesHighFanOut(reaching)

	graph.SortBlindSpots(c.BlindSpots)
	// Collect annotations once per (Site, Kind) in the now-sorted blind-spot order,
	// so the card's context is deterministic and a seam with several blind spots
	// (e.g. two external handoffs) does not repeat its shared annotation.
	c.Annotations = ix.DistinctAnnotationsAt(c.BlindSpots)
	return c, nil
}

// bindingRules names the rules that demonstrably bind fqn, with the same
// matchers the checks themselves use.
func bindingRules(ix *graph.Index, p *policy.Policy, fqn string, c *Card) []string {
	var out []string

	// Graph-borne facts bind regardless of policy.
	for _, o := range ix.Obligations() {
		if o.Fn == fqn {
			out = append(out, fmt.Sprintf("obligation %s (%s): %s at %s", o.Rule, o.Kind, o.Status, o.Site))
		}
	}
	for _, f := range ix.EffectOrder() {
		if f.Fn == fqn {
			mode := "can precede"
			if f.Always {
				mode = "always precedes"
			}
			out = append(out, fmt.Sprintf("partial-effect: %s %s the fallible call to %s", f.Effect, mode, fitness.ShortName(f.Callee)))
		}
	}

	if p != nil {
		if layer := p.LayerOf(fitness.PkgOf(fqn)); layer != "" {
			c.Layer = layer
			out = append(out, fmt.Sprintf("layering: package is in layer %q — calls may descend one layer, never skip or rise", layer))
		}
		for _, r := range p.MustNotReach {
			if fitness.MatchesAny(fqn, r.From) {
				out = append(out, fmt.Sprintf("must_not_reach %s: this function must never reach %s", r.Name, strings.Join(r.To, ", ")))
			}
		}
		for _, r := range p.MustPassThrough {
			if fitness.MatchesAny(fqn, r.Through) {
				out = append(out, fmt.Sprintf("must_pass_through %s: this function is the waypoint guarding %s", r.Name, strings.Join(r.To, ", ")))
			}
		}
		for _, r := range p.NoConcurrentReach {
			if fitness.MatchesAny(fqn, r.To) {
				out = append(out, fmt.Sprintf("no_concurrent_reach %s: must not be reached on a concurrent path", r.Name))
			}
		}
		// IsRoute is the enforcer's own route predicate — the card must not claim
		// the budget binds a caller-less function main-package startup that
		// checkIOBudget will never charge (H-8).
		if p.IOBudget != nil && fitness.IsRoute(p, ix, fqn) {
			out = append(out, fmt.Sprintf("io_budget: routes may reach at most %d external write(s)", p.IOBudget.MaxWritesPerRoute))
		}
	}
	sort.Strings(out)
	return dedupeSorted(out)
}

// dedupeSorted collapses adjacent identical lines in a sorted slice. Several
// effect_order facts that differ only by call site (e.g. many <dynamic> publish
// sites that all precede the same fallible callee) render to the SAME binding
// line; the card states each rule once rather than printing the fact N times and
// overstating its "Binding rules (N)" count. The facts keep their distinct sites
// in the graph — this is presentation only.
func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// Render is the human/agent-facing text card.
func (c Card) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", c.FQN)
	if c.Sig != "" {
		fmt.Fprintf(&b, "  %s\n", c.Sig)
	}
	if c.Layer != "" {
		fmt.Fprintf(&b, "  tier %d · layer %s\n", c.Tier, c.Layer)
	} else {
		fmt.Fprintf(&b, "  tier %d\n", c.Tier)
	}
	b.WriteString("\n")
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s (%d)\n", title, len(items))
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
		b.WriteString("\n")
	}
	section("Binding rules — these gate any edit here", c.Binding)
	section("Callers", c.Callers)
	section("Callees", c.Callees)
	entryTitle := "Live behind entrypoints"
	if c.CoverOverApprox {
		// The cover crossed a HighFanOut dispatch — the count is an upper bound.
		entryTitle = "Live behind entrypoints ≤ (over-approx via dispatch)"
	}
	section(entryTitle, c.Entrypoints)
	section("Reachable boundary effects", c.Effects)
	if len(c.BlindSpots) > 0 {
		fmt.Fprintf(&b, "🕳️  Blind spots touching this card's claims (%d)\n", len(c.BlindSpots))
		graph.WriteBlindSpots(&b, c.BlindSpots, c.Annotations, func(s graph.BlindSpot) string {
			return "- " + s.Kind + " " + s.Site
		})
	}
	return b.String()
}
