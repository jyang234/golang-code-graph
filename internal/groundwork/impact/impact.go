// Package impact assembles the incident-triage card: given suspect functions,
// the bidirectional blast radius an incident responder needs — which
// entrypoints are implicated (reverse reach), which external effects are in
// play (forward reach), and where the graph's own knowledge stops being sound
// (blind spots on any traversed path). It is the IT-0 engine from the
// incident-triage plan: pure composition over the existing graph index, a pure
// function of (graph, suspects), no policy and no verdict — the card is
// evidence, not judgment.
//
// The honest limits ride with the card: a static blast radius is the MAP (what
// the suspects COULD touch), not the route actually taken. It scopes the hunt;
// the incident's own trace locates the divergence (flowmap behavior ingest).
package impact

import (
	"fmt"
	"sort"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Card is the triage evidence for one suspect set. Every field is sorted and
// derived from the graph alone, so identical inputs render identical cards.
type Card struct {
	Suspects    []string          `json:"suspects"`
	Entrypoints []string          `json:"entrypoints,omitempty"` // implicated routes: entrypoint cover of the suspects
	Callers     []string          `json:"callers,omitempty"`     // reverse reach (who can be affected upstream)
	Effects     []string          `json:"effects,omitempty"`     // boundary effects reachable from the suspects
	BlindSpots  []graph.BlindSpot `json:"blind_spots,omitempty"` // gaps on any traversed path — where the card's claims stop being sound

	// Fault marks the what-if framing: the suspects are HYPOTHESIZED to be
	// failing, and the card reads as fault propagation — entrypoints degraded,
	// effects that may not have happened. Same evidence, same determinism; only
	// the question differs ("what if these fail" vs "what are these").
	Fault bool `json:"fault,omitempty"`

	// The partial-effect answer (IT-3), fault mode only: external effects that
	// can have already happened when a suspect faults — read off flowmap's
	// effect_order facts. Certainly = the effect dominates the fault site (it
	// happened on ANY path reaching the fault); Possibly = a path exists where
	// it happened first. This is the inconsistent-state lead the responder is
	// after: "loan.approved published, charge failed".
	PossiblyCommitted  []string `json:"possibly_committed,omitempty"`
	CertainlyCommitted []string `json:"certainly_committed,omitempty"`
}

// ForFault assembles the what-if card: mark the suspects as failing and read
// the blast radius off the same index walks. For an event symptom the resolver
// already returns both the publishers (who should emit) and the consumer
// registrars (who is now starved), so both directions of "event T missing"
// are in the suspect set.
func ForFault(ix *graph.Index, fqns []string) Card {
	c := ForNodes(ix, fqns)
	c.Fault = true

	// Partial effects: a fact applies when its fallible callee is one of the
	// hypothesized-failing suspects — the effect named by the fact may already
	// be committed when that call faults.
	suspect := setutil.StringSet(c.Suspects)
	possibly, certainly := map[string]bool{}, map[string]bool{}
	for _, f := range ix.EffectOrder() {
		if !suspect[f.Callee] {
			continue
		}
		entry := fmt.Sprintf("%s — before the fault at %s in %s", f.Effect, f.CalleeSite, fitness.ShortName(f.Fn))
		if f.Always {
			certainly[entry] = true
		} else {
			possibly[entry] = true
		}
	}
	c.PossiblyCommitted = setutil.SortedKeys(possibly)
	c.CertainlyCommitted = setutil.SortedKeys(certainly)
	return c
}

// ForNodes assembles the card for a set of suspect function FQNs.
func ForNodes(ix *graph.Index, fqns []string) Card {
	suspects := setutil.SortedKeys(setutil.StringSet(fqns))

	callers := ix.Reaching(suspects...)
	// Entrypoint cover for the whole suspect set in one pass: a source covers
	// a suspect iff it reaches one (or is one) — Sources ∩ (callers ∪ suspects).
	// Per-suspect EntrypointCover would redo a full reverse BFS per suspect.
	inCover := setutil.StringSet(callers)
	for _, s := range suspects {
		inCover[s] = true
	}
	entry := map[string]bool{}
	for _, src := range ix.Sources() {
		if inCover[src] {
			entry[src] = true
		}
	}

	forward := ix.Reachable(suspects...)
	cone := setutil.StringSet(suspects)
	for _, fn := range forward {
		cone[fn] = true
	}
	coneSorted := setutil.SortedKeys(cone)
	coneEffects := ix.Effects(coneSorted...) // gathered once: the effects section AND the dynamic probe read it
	effects := map[string]bool{}
	for _, e := range coneEffects {
		effects[e.To] = true
	}

	// Blind spots on any traversed node (function- or package-level), plus
	// dynamic boundary effects in the forward cone: the frontier where the
	// card's reachability claims are no longer sound.
	traversed := setutil.StringSet(callers)
	for fn := range cone {
		traversed[fn] = true
	}
	var blind []graph.BlindSpot
	seen := map[string]bool{}
	addBlind := func(bs []graph.BlindSpot) {
		for _, b := range bs {
			k := b.Kind + "\x00" + b.Site
			if !seen[k] {
				seen[k] = true
				blind = append(blind, b)
			}
		}
	}
	for _, fn := range setutil.SortedKeys(traversed) {
		addBlind(ix.BlindSpotsAt(fn))
		addBlind(ix.BlindSpotsAt(fitness.PkgOf(fn)))
	}
	for _, e := range coneEffects {
		if e.IsDynamic() {
			addBlind([]graph.BlindSpot{{Kind: "DynamicEffect", Site: e.From, Detail: e.To}})
		}
	}
	sort.Slice(blind, func(i, j int) bool {
		if blind[i].Kind != blind[j].Kind {
			return blind[i].Kind < blind[j].Kind
		}
		return blind[i].Site < blind[j].Site
	})

	return Card{
		Suspects:    suspects,
		Entrypoints: setutil.SortedKeys(entry),
		Callers:     callers,
		Effects:     setutil.SortedKeys(effects),
		BlindSpots:  blind,
	}
}
