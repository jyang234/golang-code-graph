package impeach

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// Severance localization (plan §6) answers WHERE the static over-approximation
// lost the observed effect, by projecting the observed (entry → effect) path onto
// the graph and finding where static cannot reproduce it. It resolves at the
// highest level the corpus supports (§7 harness levels): L0 uses only the entry and
// effect anchors (coarse), L1 also maps the causal path's internal spans through
// their `flowmap.fqn` tags to a PRECISE node. The result is a Site plus the
// known/unknown frontier sort (§6) — disclosure-only at this phase, never enacted
// (the repair-writing loop is Phase 4, §8).
//
// Soundness is INVARIANT across levels (§7): a coarser level yields a coarser Site,
// never a wrong one — every anchor is a real graph node (or the entry registration
// literal), and the proof obligation refuses to fabricate a seam where none exists.
// The Kind and the proof obligation are computed by ONE shared mechanism (localize)
// for both levels, so they can never disagree on capture richness; only the Site's
// resolution changes. Precision is a dial; soundness is invariant.

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
	// emitter IS a graph node, but no discovered root reaches it — the break is a
	// dispatch seam upstream of the emitter. Site is the precise severed path node
	// at L1, or the entry function (the coarse upstream anchor) at L0.
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

// Severance is the localization attached to a candidate witness (§6): the
// severance Site (coarse at L0, precise at L1), the flavor of the break, and the
// known/unknown frontier sort. It is a pure function of (witness, graph) — every
// field resolves on intrinsic graph data (entrypoint join, emitter node,
// reachability, frontier markers), never on arrival order — so it rides the
// byte-identical report.
type Severance struct {
	// Site is the severance point: the entry registration literal (a missed root),
	// the entry function or the precise severed path node (a severed emitter /
	// unmodeled effect, coarse at L0 / precise at L1), or "" when the Kind is
	// SeveranceNone (the proof obligation failed — no fabricated seam). The
	// repair-proposal loop (§8) will target this; Phase 2/3 only records it.
	Site string `json:"site"`

	// Kind is the break flavor (SeveranceMissedRoot | SeveranceSeveredEmitter |
	// SeveranceUnmodeledEffect | SeveranceNone), classified by severanceKind from
	// whether the entry is a discovered root and whether an emitter is modeled —
	// independent of the resolution level, so L0 and L1 never disagree (§6).
	Kind string `json:"kind"`

	// FrontierKnown sorts the value (§6): true when a static frontier marker or a
	// disclosed blind spot already covers Site — behavior confirms a DISCLOSED seam
	// (a "the negative should have respected the frontier" bug). false is the
	// high-value discovery — an UNDISCLOSED blind spot static did not know it had.
	FrontierKnown bool `json:"frontier_known"`

	// Anchors is the ordered anchor chain the walk mapped onto the graph — at L0
	// the discovered entry function then the effect emitter, at L1 the precise node
	// chain entry→…→emitter resolved through the span map (§7). An entry that did
	// not map (missed root) is omitted, and an unmodeled effect contributes no
	// emitter, so the chain length itself is a signal (§6's EntryDiscovered /
	// missing-emitter distinction).
	Anchors []string `json:"anchors,omitempty"`

	// Level is the harness-investment level the Site was resolved at (§7): "L0"
	// (entry+effect anchors only — coarse but sound) or "L1" (precise, resolved
	// through `flowmap.fqn` tags on the causal path). Recorded as provenance: the
	// SAME report shape carries either, and a reader knows the Site's resolution.
	Level string `json:"level"`

	// AbsentFromGraph is the sharp L1 signal (§7): an internal span whose FQN tag
	// keys to a function the graph has NO node for — a directly localized missing
	// node, sharper than the walk. It is a WEAK HINT at L1 (it is sound only once
	// canonFQN ⊥-symmetry is fuzz-proven, §12.5), so it rides as disclosure beside
	// the walk's Site, never replacing it. "" when no internal tag was absent.
	AbsentFromGraph string `json:"absent_from_graph,omitempty"`
}

// Harness-investment levels (§7), recorded as Severance.Level provenance.
const (
	LevelL0 = "L0"
	LevelL1 = "L1"
)

// localizer holds the per-audit state the severance walk shares across every
// candidate: the graph, the span↔node index, the root-reachable cone, and a memo
// of the reaches-emitter set keyed by effect (the one per-candidate reverse BFS,
// hoisted so candidates of one effect compute it once). Built once per Audit.
type localizer struct {
	ix        *graph.Index
	nx        *nodeIndex
	rootReach map[string]bool            // reachable from any discovered root (roots included)
	emitReach map[string]map[string]bool // effect key -> {nodes that can reach an emitter}
}

func newLocalizer(ix *graph.Index) *localizer {
	return &localizer{
		ix:        ix,
		nx:        buildNodeIndex(ix),
		rootReach: rootReachOf(ix),
		emitReach: map[string]map[string]bool{},
	}
}

// localize runs the severance walk (§6) over one candidate and returns its
// Severance plus whether the proof obligation HELD (a real contradiction exists).
// ok == false (Kind SeveranceNone) means the effect was statically reproducible
// from the observed entry — the Claim was a mis-read, so the caller must disclose
// a self-inconsistency rather than localize a seam (§6: never a fabricated Site).
//
// SHARED mechanism, precision DIAL (§7): the proof obligation, the Kind, the Site
// fallback, and the anchor assembly are computed ONCE here for both levels; the
// only thing the level changes is whether the Site is the precise severed PATH
// NODE (L1, when `flowmap.fqn` tags resolve) or the coarse entry/registration
// anchor (L0). So a tagged and an untagged corpus classify a candidate identically
// — same Kind, same proof-obligation verdict — and differ only in Site resolution.
//
// discovered is also returned so the caller can stamp Observation.EntryDiscovered
// without re-running the entrypoint join.
func (lz *localizer) localize(w Witness) (sev Severance, discovered, ok bool) {
	emitters := staticEmitters(lz.ix, w.Effect)
	entryFn := mapEntry(lz.ix, w.Observed.Entry)
	discovered = entryFn != ""
	reachAbsent := w.Claim.Reachability == ReachAbsent

	// The precise L1 Site, when the causal path offers a signal. level records the
	// resolution; severedSite/pathAnchors/absentHint are empty at L0.
	severedSite, pathAnchors, absentHint, l1 := lz.walkL1(w.chain, emitters)
	level := LevelL0
	if l1 {
		level = LevelL1
	}

	// Proof obligation (ONE per-entry check, both levels, §6): if THIS discovered
	// entry statically reaches an emitter, the effect is reproducible — the
	// unreachable Claim was a mis-read, so disclose a self-inconsistency, never a
	// fabricated seam. It cannot trip for a real candidate (a reached emitter is
	// CONFIRMED-LIVE, filtered upstream); the search IS the verification. Skipped
	// for a missed root (no entry to reproduce from) and a bare absent effect (no
	// emitter to reach).
	if discovered && len(emitters) > 0 {
		entryReach := reachSetOf(lz.ix, []string{entryFn})
		for _, e := range emitters {
			if entryReach[e] {
				return Severance{Kind: SeveranceNone, Level: level}, discovered, false
			}
		}
	}

	// Site: the precise severed node when the L1 walk found one, else the coarse
	// anchor — the entry function when discovered, the registration literal (the
	// missed-root seam) otherwise.
	site := severedSite
	if site == "" {
		if discovered {
			site = entryFn
		} else {
			site = w.Observed.Entry
		}
	}

	sev = Severance{
		Site:            site,
		Kind:            severanceKind(discovered, reachAbsent),
		FrontierKnown:   frontierKnown(lz.ix, site),
		Anchors:         assembleAnchors(discovered, entryFn, pathAnchors, emitters),
		Level:           level,
		AbsentFromGraph: absentHint,
	}
	return sev, discovered, true
}

// severanceKind classifies the contradiction's shape from the two facts that
// determine it — whether the entry is a discovered root and whether the effect is
// unmodeled — INDEPENDENT of the resolution level (§6's three flavors). One
// source, so a tagged and an untagged corpus can never disagree on the Kind.
func severanceKind(discovered, reachAbsent bool) string {
	switch {
	case !discovered:
		return SeveranceMissedRoot // the entry registration site is the seam
	case reachAbsent:
		return SeveranceUnmodeledEffect // a discovered root, but no emitter modeled
	default:
		return SeveranceSeveredEmitter // a modeled emitter no root reaches
	}
}

// assembleAnchors builds the ordered anchor chain entry→…→emitter: the discovered
// entry function (omitted for a missed root), the L1 path nodes (empty at L0), then
// the modeled emitters (empty for an unmodeled effect). The chain length itself is
// the §6 signal (no entry ⇒ missed root; no emitter ⇒ unmodeled), kept honest by
// this ONE assembler instead of a per-branch copy.
func assembleAnchors(discovered bool, entryFn string, pathAnchors, emitters []string) []string {
	var a []string
	if discovered {
		a = append(a, entryFn)
	}
	a = append(a, pathAnchors...)
	a = append(a, emitters...)
	return a
}

// walkL1 projects the causal span chain onto the graph (§7) and returns the
// precise severance: the FIRST mapped path node that is severed from every root
// AND can reach an emitter (the outermost point on the OBSERVED path where static
// lost the effect — a node Site a blind_spot self-extinguishes, §6/§8), the mapped
// anchor chain, the sharp `absent-from-graph` hint, and whether ANY L1 signal was
// present. l1 == false means the corpus offers no signal, so the caller resolves
// the Site coarsely (L0). It never classifies — Kind and the proof obligation are
// the caller's shared mechanism.
func (lz *localizer) walkL1(chain []*ir.CanonicalSpan, emitters []string) (severedSite string, anchors []string, absentHint string, l1 bool) {
	reachesEmitter := lz.reachesEmitterSet(emitters)
	for _, s := range pathSpans(chain) {
		a := lz.nx.mapInternal(s)
		switch a.Outcome {
		case MapMapped:
			l1 = true
			anchors = append(anchors, a.Node)
			if severedSite == "" && !lz.rootReach[a.Node] && reachesEmitter[a.Node] {
				severedSite = a.Node
			}
		case MapAbsentFromGraph:
			l1 = true
			if absentHint == "" {
				absentHint = a.Tag
			}
		}
	}
	return severedSite, anchors, absentHint, l1
}

// reachesEmitterSet is the set of nodes that can reach a modeled emitter (the
// emitters included), memoized by effect: candidates of one effect share the
// emitters, so the O(V+E) reverse BFS runs once per distinct effect rather than
// once per candidate. The key is the canonical effect (the emitters' identity), so
// the memo is sound.
func (lz *localizer) reachesEmitterSet(emitters []string) map[string]bool {
	key := strings.Join(emitters, "\x00")
	if m, ok := lz.emitReach[key]; ok {
		return m
	}
	m := map[string]bool{}
	for _, e := range append(lz.ix.Reaching(emitters...), emitters...) {
		m[e] = true
	}
	lz.emitReach[key] = m
	return m
}

// pathSpans is the causal chain with the EFFECT (terminal) span dropped — every
// span whose `flowmap.fqn` tag the L1 map may anchor (§7's "internal" class, plus
// the ROOT span). The effect span is excluded because its emitter is recovered
// from the static label (staticEmitters), and tagging it would wrongly localize
// the Site onto the emitter itself rather than the upstream seam. The root IS kept:
// when the entry route does not map (a missed root), the root span's own FQN tag is
// the only signal that can pin the missed-root function to a precise node.
func pathSpans(chain []*ir.CanonicalSpan) []*ir.CanonicalSpan {
	if len(chain) <= 1 {
		return nil
	}
	return chain[:len(chain)-1]
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
