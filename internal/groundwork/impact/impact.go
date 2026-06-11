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
}

// ForNodes assembles the card for a set of suspect function FQNs.
func ForNodes(ix *graph.Index, fqns []string) Card {
	suspects := setutil.SortedKeys(setutil.StringSet(fqns))

	callers := ix.Reaching(suspects...)
	entry := map[string]bool{}
	for _, s := range suspects {
		for _, ep := range ix.EntrypointCover(s) {
			entry[ep] = true
		}
	}

	forward := ix.Reachable(suspects...)
	cone := setutil.StringSet(suspects)
	for _, fn := range forward {
		cone[fn] = true
	}
	effects := map[string]bool{}
	for _, e := range ix.Effects(setutil.SortedKeys(cone)...) {
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
	for _, e := range ix.Effects(setutil.SortedKeys(cone)...) {
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
