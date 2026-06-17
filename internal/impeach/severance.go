package impeach

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// Severance localization (plan §6), at the L0 resolution (§7 harness levels):
// entry+effect anchors only, no internal-span FQN reconciliation. It answers
// WHERE the static over-approximation lost the observed effect, by projecting the
// coarse observed (entry → effect) anchor pair onto the graph and finding the
// first hop static cannot reproduce. The result is a coarse Site plus the
// known/unknown frontier sort (§6) — disclosure-only at this phase, never enacted
// (the repair-writing loop is Phase 4, §8).
//
// L0 soundness (§7): the Site is COARSER than L1/L2 (it cannot point inside the
// handler cone), but it is never a GUESS — every anchor is a real graph node (or
// the entry registration literal), and the proof obligation below refuses to
// fabricate a seam where none exists. Precision is a dial; soundness is invariant.

// Severance kinds (§6, the "three flavors classified for free"). Each names the
// shape of the broken link the L0 walk found, so a reader knows what to repair
// without re-deriving it.
const (
	// SeveranceMissedRoot: the observed entry is not a discovered entrypoint, so
	// the graph models no root for this flow at all — the entry REGISTRATION site
	// is the seam (an unhinted router, a framework registration static did not
	// model). Site is the entry route literal; EntryDiscovered is false.
	SeveranceMissedRoot = "missed-root"

	// SeveranceSeveredEmitter: the entry IS a discovered root and the effect's
	// emitter IS a graph node, but no discovered root reaches it — the break is
	// upstream of the emitter, a dispatch seam somewhere on entry→emitter that L0
	// cannot resolve finer. Site is the entry function (the upstream anchor).
	SeveranceSeveredEmitter = "severed-emitter"

	// SeveranceUnmodeledEffect: the graph models NO emitter for the effect at all
	// (the ReachAbsent candidate), reached from a discovered entry — static could
	// not model or label the effect itself. Site is the entry function whose cone
	// the effect escaped.
	SeveranceUnmodeledEffect = "unmodeled-effect"

	// SeveranceNone: the proof obligation FAILED — the effect is statically
	// reproducible along the observed anchors, so the "unreachable" Claim was a
	// mis-read and there is no real contradiction to localize (§6). Recorded with
	// NO Site (never a fabricated seam); Phase 5 verdict integration must treat
	// this as non-impeaching. It cannot arise from a Phase-0 candidate by
	// construction (an emitter a discovered root reaches is CONFIRMED-LIVE, not a
	// candidate) — the rung is the explicit fail-closed guard if it ever does.
	SeveranceNone = "no-severance"
)

// Severance is the L0 localization attached to a candidate witness (§6): the
// coarse severance Site, the flavor of the break, and the known/unknown frontier
// sort. It is a pure function of (witness, graph) — every field resolves on
// intrinsic graph data (entrypoint join, emitter node, reachability, frontier
// markers), never on arrival order — so it rides the byte-identical report.
type Severance struct {
	// Site is the coarse L0 severance point: the entry registration literal (a
	// missed root), the entry function (a severed emitter / unmodeled effect), or
	// "" (the proof obligation failed — no fabricated seam). The repair-proposal
	// loop (§8) will target this; Phase 2 only records it.
	Site string `json:"site"`

	// Kind is the break flavor (SeveranceMissedRoot | SeveranceSeveredEmitter |
	// SeveranceUnmodeledEffect | SeveranceNone), classified for free from whether
	// the entry mapped and whether an emitter exists (§6).
	Kind string `json:"kind"`

	// FrontierKnown sorts the value (§6): true when a static frontier marker or a
	// disclosed blind spot already covers Site — behavior confirms a DISCLOSED seam
	// (a "the negative should have respected the frontier" bug). false is the
	// high-value discovery — an UNDISCLOSED blind spot static did not know it had.
	FrontierKnown bool `json:"frontier_known"`

	// Anchors is the ordered coarse L0 anchor chain the walk mapped onto the graph
	// (the discovered entry function, then the effect emitter), the run-independent
	// evidence Site was derived from. An entry that did not map (missed root) is
	// omitted, and an unmodeled effect contributes no emitter, so the chain length
	// itself is a signal (§6's EntryDiscovered / missing-emitter distinction).
	Anchors []string `json:"anchors,omitempty"`
}

// localize runs the L0 severance walk (§6) over one candidate and returns its
// Severance plus whether the proof obligation HELD (a real contradiction exists).
// ok == false (Kind SeveranceNone) means the effect was statically reproducible
// from the observed entry — the Claim was a mis-read, so the caller must disclose
// a self-inconsistency rather than localize a seam (§6: never a fabricated Site).
//
// discovered is also returned so the caller can stamp Observation.EntryDiscovered
// without re-running the entrypoint join.
func localize(w Witness, ix *graph.Index) (sev Severance, discovered, ok bool) {
	emitters := staticEmitters(ix, w.Effect)
	entryFn := mapEntry(ix, w.Observed.Entry)
	discovered = entryFn != ""

	switch {
	case !discovered:
		// The entry is not a graph root: the registration site is the seam,
		// regardless of whether an emitter is modeled (§6, EntryDiscovered=false).
		// The emitter (if any) is unreachable from every discovered root by the
		// candidate's construction, so the missed root is the real contradiction.
		sev = Severance{Site: w.Observed.Entry, Kind: SeveranceMissedRoot, Anchors: emitters}
	case w.Claim.Reachability == ReachAbsent:
		// A discovered root, but the graph models no emitter at all — the effect
		// itself is unmodeled (§6, "break at the emitter" with no emitter node).
		sev = Severance{Site: entryFn, Kind: SeveranceUnmodeledEffect, Anchors: []string{entryFn}}
	default:
		// A discovered root AND a modeled emitter, but no root reaches it. The
		// proof obligation: confirm THIS entry does not reach any emitter either.
		// It cannot, by construction (a reached emitter is CONFIRMED-LIVE), but the
		// search IS the verification — a reproducible effect here is a self-
		// inconsistency, not an impeachment (§6).
		reach := reachSetOf(ix, []string{entryFn})
		for _, e := range emitters {
			if reach[e] {
				return Severance{Site: "", Kind: SeveranceNone}, discovered, false
			}
		}
		anchors := []string{entryFn}
		anchors = append(anchors, emitters...)
		sev = Severance{Site: entryFn, Kind: SeveranceSeveredEmitter, Anchors: anchors}
	}

	sev.FrontierKnown = frontierKnown(ix, sev.Site)
	return sev, discovered, true
}

// staticEmitters returns the sorted, deduped first-party FQNs the graph models as
// emitting the effect key — the bus PUBLISH or DB op the key names. Empty when the
// graph models no emitter (a ReachAbsent candidate). Reused decoders (BusEffects /
// DBEffects) keep the boundary-label vocabulary with the schema owner, and the key
// reconciliation goes through the one-source DBEffectKey / bus op key, so an
// emitter is matched like-with-like (the same parity the join itself relies on).
func staticEmitters(ix *graph.Index, effect string) []string {
	seen := map[string]bool{}
	switch {
	case isDBKey(effect):
		dbEffs, _ := ix.DBEffects()
		for _, de := range dbEffs {
			if DBEffectKey(de.Op, de.Table) == effect {
				seen[de.From] = true
			}
		}
	case isBusKey(effect):
		busEffs, _ := ix.BusEffects()
		for _, be := range busEffs {
			if be.Op == graph.BusPublish && graph.BusPublish+" "+be.Event == effect {
				seen[be.From] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for fqn := range seen {
		out = append(out, fqn)
	}
	sort.Strings(out)
	return out
}

// mapEntry projects the observed entry op onto a discovered entrypoint and returns
// its handler FQN, or "" when the graph models no such root (a missed root, §6).
// The reconciliation is structural: the canonical entry op is "HTTP <METHOD>
// <route>" or "CONSUME <topic>" (opkey.Of for a server/consumer span), while an
// Entrypoint.Name is the bare "<METHOD> <route>" / "<topic>" — so the op key's
// prefix is stripped and the remainder matched against Name. It never guesses: an
// op with no recognized entry prefix, or no matching entrypoint, yields "".
func mapEntry(ix *graph.Index, entryOp string) string {
	var name string
	switch {
	case strings.HasPrefix(entryOp, opkey.HTTPPrefix):
		name = strings.TrimPrefix(entryOp, opkey.HTTPPrefix)
	case strings.HasPrefix(entryOp, opkey.ConsumePrefix):
		name = strings.TrimPrefix(entryOp, opkey.ConsumePrefix)
	default:
		return ""
	}
	for _, ep := range ix.Entrypoints() {
		if ep.Name == name && ep.Fn != "" {
			return ep.Fn
		}
	}
	return ""
}

// frontierKnown reports whether the graph already DISCLOSES a seam at site — a
// frontier marker (by site or reclaim owner) or a blind spot there. This is the
// §14-D markerAt lookup over the shipped frontier section (Index.Frontier) plus
// the blind-spot manifest. A known site means behavior confirms a seam static
// already admitted (lower value); an unknown site is the undisclosed blind spot
// the cell exists to discover (§3). The entry-literal Site of a missed root is
// never a graph node, so it is correctly unknown — an undisclosed missed root.
func frontierKnown(ix *graph.Index, site string) bool {
	if site == "" {
		return false
	}
	if len(ix.BlindSpotsAt(site)) > 0 {
		return true
	}
	if fr := ix.Frontier(); fr != nil {
		for _, m := range fr.Markers {
			if m.Site == site || m.Owner == site {
				return true
			}
		}
	}
	return false
}
