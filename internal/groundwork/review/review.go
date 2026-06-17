package review

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// Review computes the MR review artifact from the base and branch graphs under a
// policy. It is a pure function: identical inputs always yield an identical
// artifact, digest included — which is what makes the verdict a hard, repeatable
// gate rather than an opinion.
//
// The graphs MUST come from trusted CI (flowmap run on the respective code). An
// agent that supplies its own branch graph forges any verdict by omitting an
// edge; groundwork cannot detect that, and does not try to — the trust boundary
// is around graph generation, not here.
func Review(p *policy.Policy, base, branch *graph.Graph) Artifact {
	baseIx, branchIx := graph.NewIndex(base), graph.NewIndex(branch)
	d := diffGraphs(base, branch)

	newViolations, newCautions, standingCautions, baseRW, branchRW := newFindings(p, baseIx, branchIx)

	a := Artifact{
		Service:          p.Service,
		Shape:            d.shape(),
		Touches:          d.pkgDeltas(),
		NewViolations:    newViolations,
		Contract:         contractChanges(baseIx, branchIx),
		Effects:          ioEffects(d),
		RouteIO:          routeIODeltas(p, baseIx, branchIx, baseRW, branchRW),
		NewWriteTargets:  newWriteTargets(p, base, branch),
		Reach:            reachExisting(d, baseIx, branchIx),
		NewCautions:      newCautions,
		StandingCautions: standingCautions,
		NewBlindSpots:    newBlindSpots(p, base, branch),
		DBLabelDrift:     dbLabelDrift(base, branch),
		Algo:             branch.Algo,
		Caveats:          provenanceCaveats(p.Substrate, base, branch),
	}
	a.Verdict = verdict(p, d, &a)
	a.Digest = digestOf(a)
	return a
}

// verdict applies the three-valued rule: a new violation, a breaking contract
// change, or a policy-gated ratchet finding (blind spots, write targets)
// blocks; otherwise the artifact abstains only when it carries NO signal at
// all. Abstention is a statement about the whole artifact, not the node/edge
// delta: graph sections beyond structure (obligations, blind spots) change on
// body-only edits, and NO-STRUCTURAL-SIGNAL's render says "the graph has
// nothing to say" — which must never hide a new caution or disclosure. It
// reads the assembled artifact by field name, so the next gated section needs
// no signature change and cannot be transposed with an existing one.
func verdict(p *policy.Policy, d graphDelta, a *Artifact) Verdict {
	if len(a.NewViolations) > 0 || anyBreaking(a.Contract) {
		return Block
	}
	if p.GatesBlindSpots() && len(a.NewBlindSpots) > 0 {
		return Block
	}
	if p.GatesEffects() && len(a.NewWriteTargets) > 0 {
		return Block
	}
	// StandingCautions is deliberately NOT a signal here: it holds identically on
	// base and branch, so it says nothing about THIS change — a body-only change
	// with only a standing caution must still abstain (NO-STRUCTURAL-SIGNAL), and
	// the abstain render surfaces the caution anyway (R1). Adding it to hasSignal
	// would misreport such a change as STRUCTURALLY-CLEAR — "the graph says it's
	// fine" — exactly where logic review matters most. Leave it out.
	// Contract changes ARE a signal: entrypoints come from the graph's Entrypoints
	// join, not its nodes/edges, so an additive (non-breaking) route can leave
	// d.empty() true while a real interface disclosure exists. Omitting it would
	// let NO-STRUCTURAL-SIGNAL's render hide that new route — the one thing the
	// abstain verdict promises never to do. (Breaking changes already Block above.)
	hasSignal := !d.empty() || len(a.Contract) > 0 || len(a.NewCautions) > 0 || len(a.NewBlindSpots) > 0 || len(a.NewWriteTargets) > 0 || a.DBLabelDrift != nil
	if !hasSignal {
		return NoStructuralSignal
	}
	return StructurallyClear
}

// newBlindSpots returns the branch's blind spots absent from the base and not
// covered by the policy's allow-list — the blind-spot ratchet's drift. Identity
// is (kind, site); Detail is carried for display but never keys the diff.
//
// A human-RATIFIED kind (blindspots.Kind.Ratified — currently ImpeachmentSeam) is
// excluded here by construction: it is not detected drift but a CODEOWNER-gated
// config declaration (config.static.declaredBlindSpots, merged in
// graphio.mergeDeclaredBlindSpots). It exists only because a human already reviewed
// and ratified the seam, so treating its appearance on the branch as
// undisclosed-dynamism drift would make the impeachment enactment self-defeating —
// every ratified seam would immediately re-block the very change that ratified it.
// The seam's behavioral meaning is surfaced on the impeachment path (the audit report
// and verdict), not the blind-spot ratchet; this section is strictly the drift
// channel, and a reviewed seam is not drift. The skip consults the one shared
// Ratified predicate (also used by frontier.Classify), so a future declared kind is
// excluded in both places from a single edit.
func newBlindSpots(p *policy.Policy, base, branch *graph.Graph) []BlindSpotDelta {
	key := func(kind, site string) string { return kind + "\x00" + site }
	baseKeys := map[string]bool{}
	for _, s := range base.BlindSpots {
		baseKeys[key(s.Kind, s.Site)] = true
	}
	seen := map[string]bool{}
	var out []BlindSpotDelta
	for _, s := range branch.BlindSpots {
		k := key(s.Kind, s.Site)
		if baseKeys[k] || seen[k] || blindspots.Kind(s.Kind).Ratified() || p.BlindSpotRatchet.Allows(s.Kind, s.Site) {
			continue
		}
		seen[k] = true
		out = append(out, BlindSpotDelta{Kind: s.Kind, Site: s.Site, Detail: s.Detail})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Site < out[j].Site
	})
	return out
}

// dbLabelDrift counts DB effect edges whose SQL verb the labeler could not read,
// base and branch, and returns the pair only when the branch has MORE — the
// write-surface family's blind fraction grew. It is computed over the whole graph
// (every such edge), not the io_budget routes, so the disclosure stands even
// when no io_budget is declared: it is a graph-health metric, not a rule. A
// decrease or no change is silent (fidelity holding is the expected state).
func dbLabelDrift(base, branch *graph.Graph) *DBLabelDrift {
	count := func(g *graph.Graph) int {
		n := 0
		for _, e := range g.Edges {
			if _, ok := fitness.UnclassifiedDBLabel(e); ok {
				n++
			}
		}
		return n
	}
	b, br := count(base), count(branch)
	if br <= b {
		return nil
	}
	return &DBLabelDrift{Base: b, Branch: br}
}

// newFindings runs fitness on both graphs and returns the findings present on
// the branch but not the base — the "report only newly-introduced" property —
// plus the branch's STANDING cautions (present on both sides) and each side's
// computed route-write map (nil when the policy has no io_budget), so the
// route-delta section reuses Check's per-route BFS.
//
// standingCautions is the absolute escape hatch for R1: a caution that holds
// identically on base and branch (the steady-state io_budget-unenforceable case
// is the load-bearing one) is suppressed by the new-findings diff forever — the
// same born-inert-is-invisible trap rule_liveness exists to defeat. The review
// surface lists it absolutely so a green gate never hides a standing "the graph
// cannot prove this" disclosure.
func newFindings(p *policy.Policy, baseIx, branchIx *graph.Index) (violations, cautions, standingCautions []Violation, baseRW, branchRW map[string]fitness.RouteIO) {
	baseRes := fitness.Check(p, baseIx)
	branchRes := fitness.Check(p, branchIx)

	baseKeys := map[string]bool{}
	for _, f := range baseRes.Findings {
		baseKeys[f.Key()] = true
	}
	for _, f := range branchRes.Findings {
		v := Violation{Rule: f.Rule, Summary: f.Summary, From: f.From, To: f.To, Detail: f.Detail}
		switch {
		case !baseKeys[f.Key()] && f.Severity == fitness.Violation:
			violations = append(violations, v)
		case !baseKeys[f.Key()]:
			cautions = append(cautions, v)
		case f.Severity == fitness.Caution:
			// Present on both sides: a standing caution the delta would suppress.
			standingCautions = append(standingCautions, v)
		}
	}
	return violations, cautions, standingCautions, baseRes.RouteWrites, branchRes.RouteWrites
}

// routeKey identifies an external entrypoint by kind AND name. Keying on the
// name alone would let an http route and a consumer topic that happen to share a
// name collapse to one entry, so removing one could be masked by the other; the
// kind keeps the two namespaces apart.
type routeKey struct{ kind, name string }

// effectKey identifies an inter-service effect target by its surface and name.
type effectKey struct{ surface, name string }

// contractChanges reports inter-service surface movement: entrypoints and
// bus/outbound effects added or removed. DB effects are excluded — the store is
// the service's own, not its contract. A removal is breaking.
//
// "Entrypoint" means a NAMED external entrypoint — an HTTP route or a consumed
// topic (the graph's Entrypoints join), NOT every graph root. A root is the
// graph's structural notion (a node with no first-party caller) and over-counts
// the contract: an unwired exported method, a closure orphaned by an
// extract-function refactor (run$4 → newHTTPServer$1), and an internal function
// left rootless when a backend is deleted (pollMessages, Queue.Acknowledge) are
// all roots but none are inter-service contract.
//
// The delta is keyed on the ROUTE NAME (ep.Name) — the HTTP method+path or
// consumed topic — NOT the handler FQN. The route name is the inter-service
// contract; the handler FQN is an implementation detail that a refactor may move
// without changing the contract. Keying on the FQN over-fired on a route handled
// by an inline closure (GET /livez registered as an anonymous func in run()):
// its handler FQN is a synthetic, position-derived name (…run$4), and an
// extract-function refactor renumbers it (run$4 → newHTTPServer$1) while the
// route name is unchanged, so the FQN-keyed delta read one route as
// removed+added — a spurious breaking contract change (field report R10).
// Name-keying subsumes the §9 orphan-root exclusion too: an internal root, an
// unwired exported method, and a refactor-orphaned non-route closure are all
// absent from the entrypoints join, so they carry no route name and never enter
// this delta — that churn is internal structure, surfaced in the node/edge delta.
//
// The effect surface is keyed the same way: on the SET of effect TARGETS (each
// published topic, consumed topic, outbound endpoint), not per emitting edge. A
// topic is "published" if ANY function publishes it, so the contract is a
// set-membership fact, not a per-emitter one. Keying per edge (on the boundary
// edge's From) over-fired on the same class as R10 — a refactor that moved the
// emitting function while the target stood read as removed+added: a renamed or
// extracted publisher, or a consolidation pointing several callers at one helper
// (in obligsvc, loan.approved is published by seven functions; collapsing them
// onto publishApproved fired six spurious breaking removals). Set-keying reports
// a removal only when the target leaves the branch entirely — the genuine
// contract break — and is independent of the effect_ratchet (newWriteTargets,
// which already keys on the write-label set), so the write ratchet is unaffected.
func contractChanges(baseIx, branchIx *graph.Index) []ContractChange {
	var out []ContractChange

	baseRoutes := externalRoutes(baseIx)
	branchRoutes := externalRoutes(branchIx)
	for k := range branchRoutes {
		if !baseRoutes[k] {
			out = append(out, ContractChange{Op: "+", Surface: "entrypoint", Name: k.name})
		}
	}
	for k := range baseRoutes {
		if !branchRoutes[k] {
			out = append(out, ContractChange{Op: "-", Surface: "entrypoint", Name: k.name, Breaking: true})
		}
	}

	baseEff := contractEffects(baseIx)
	branchEff := contractEffects(branchIx)
	for k := range branchEff {
		if !baseEff[k] {
			out = append(out, ContractChange{Op: "+", Surface: k.surface, Name: k.name})
		}
	}
	for k := range baseEff {
		if !branchEff[k] {
			out = append(out, ContractChange{Op: "-", Surface: k.surface, Name: k.name, Breaking: true})
		}
	}

	out = dedupeConsumeWithEntrypoint(out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Surface != out[j].Surface {
			return out[i].Surface < out[j].Surface
		}
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// dedupeConsumeWithEntrypoint drops a `consume` effect change when the same topic
// already moves on the `entrypoint` surface with the same op. A consumed topic is
// usually both a consumer entrypoint (the Entrypoints join) and a `bus CONSUME`
// boundary effect, so one logical consumed-topic change would otherwise report
// twice with the identical Name (entrypoint + consume). The entrypoint surface is
// the canonical named representation, so it wins; a consume effect with no matching
// entrypoint (a mid-flow consume that is not a registered entrypoint) is kept.
func dedupeConsumeWithEntrypoint(cs []ContractChange) []ContractChange {
	entry := map[[2]string]bool{}
	for _, c := range cs {
		if c.Surface == "entrypoint" {
			entry[[2]string{c.Op, c.Name}] = true
		}
	}
	out := cs[:0]
	for _, c := range cs {
		if c.Surface == "consume" && entry[[2]string{c.Op, c.Name}] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// externalRoutes returns the set of NAMED external routes a graph exposes — the
// HTTP method+path or consumed topic (ep.Name) of each http/consumer entrypoint,
// keyed by (kind, name). The route name, not the handler FQN, is the inter-service
// contract: it is stable across refactors that move a route's handler (an
// extract-function renumber of an inline-closure handler, run$4 → newHTTPServer$1),
// so a delta keyed on it reports only genuine route additions and removals, never
// handler-identity churn. A root absent from the entrypoints join (an unwired
// exported method, a refactor-orphaned non-route closure, an internal function left
// rootless by a deleted call site) contributes no name and is internal churn,
// surfaced in the node/edge delta.
//
// When the graph carries no entrypoints join at all (a pre-entrypoints flowmap, or
// a service with no detected external surface) this is empty, so no route movement
// is reported as a contract change — correct for a service with no declared external
// surface, and a deliberate trade: the precise external signal over the structural
// over-approximation that cried wolf on internal churn.
func externalRoutes(ix *graph.Index) map[routeKey]bool {
	out := map[routeKey]bool{}
	for _, ep := range ix.Entrypoints() {
		if (ep.Kind == "http" || ep.Kind == "consumer") && ep.Name != "" {
			out[routeKey{ep.Kind, ep.Name}] = true
		}
	}
	return out
}

// contractEffects returns the set of inter-service effect targets a graph
// exposes — each published topic, consumed topic, and outbound endpoint, keyed by
// (surface, name) (DB effects excluded: the store is internal, not contract).
// Like externalRoutes, this is the CONTRACT as a set-membership fact, deduped
// across every emitting edge: a topic is published if any function publishes it,
// so which function does is an implementation detail. Deduping here is what keeps
// a refactor that moves the emitter (rename, extract-function, or consolidating
// several callers onto one helper) from reading as a removed+added effect — the
// per-edge keying over-fired on exactly that, the same class as R10.
func contractEffects(ix *graph.Index) map[effectKey]bool {
	return edgeSet(ix.Edges(), func(e graph.Edge) (effectKey, bool) {
		if !e.IsBoundary() {
			return effectKey{}, false
		}
		surface, name, ok := classifyContract(e)
		return effectKey{surface, name}, ok
	})
}

// classifyContract maps a boundary effect to its inter-service surface, or
// reports ok=false for a DB effect (internal store, not contract).
func classifyContract(e graph.Edge) (surface, name string, ok bool) {
	f := strings.Fields(strings.TrimPrefix(e.To, "boundary:"))
	if len(f) < 2 {
		return "", "", false
	}
	switch f[0] {
	case "db":
		return "", "", false
	case "bus":
		switch strings.ToUpper(f[1]) {
		case "PUBLISH":
			return "publish", strings.Join(f[2:], " "), true
		case "CONSUME":
			return "consume", strings.Join(f[2:], " "), true
		}
		return "", "", false
	default:
		return "outbound", strings.Join(f, " "), true
	}
}

// ioEffects reports every external I/O effect added or removed, DB writes
// included, with the write flag.
func ioEffects(d graphDelta) []EffectChange {
	var out []EffectChange
	for _, e := range d.effectsAdded {
		out = append(out, EffectChange{Op: "+", Effect: strings.TrimPrefix(e.To, "boundary:"), Write: fitness.IsWrite(e)})
	}
	for _, e := range d.effectsRemoved {
		out = append(out, EffectChange{Op: "-", Effect: strings.TrimPrefix(e.To, "boundary:"), Write: fitness.IsWrite(e)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Effect != out[j].Effect {
			return out[i].Effect < out[j].Effect
		}
		return out[i].Op < out[j].Op
	})
	return out
}

// reachExisting returns the pre-existing entrypoints that the changed sites are
// now live behind — the blast surface the reviewer is now responsible for. New
// entrypoints introduced by the MR are reported in the contract section, not
// here; this is specifically "what that already exists is affected".
func reachExisting(d graphDelta, baseIx, branchIx *graph.Index) []string {
	existing := setutil.StringSet(baseIx.Sources())

	sites := map[string]bool{}
	for _, n := range d.nodesAdded {
		sites[n] = true
	}
	for _, e := range d.edgesAdded {
		sites[e[0]] = true
	}
	for _, e := range d.effectsAdded {
		sites[e.From] = true
	}

	// One multi-seed reverse BFS over all changed sites, then one Sources
	// scan: identical to unioning per-site EntrypointCover (a source covers a
	// site iff it reaches one or is one), without a full BFS per site.
	reach := setutil.StringSet(branchIx.Reaching(setutil.SortedKeys(sites)...))
	for site := range sites {
		reach[site] = true
	}
	hit := map[string]bool{}
	for _, src := range branchIx.Sources() {
		if existing[src] && reach[src] {
			hit[src] = true
		}
	}
	return setutil.SortedKeys(hit)
}
