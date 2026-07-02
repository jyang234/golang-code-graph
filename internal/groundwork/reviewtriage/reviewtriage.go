// Package reviewtriage is a PROTOTYPE human-reviewer triage surface: given the base
// and branch graphs of an MR, it sorts the *changed* functions into three zones for a
// reviewer drowning in diff volume, by the inverse of the tool's own confidence AND by
// what THIS diff actually moved —
//
//   - NEW BLIND (focus): the change introduces or newly reaches a blind spot — the
//     tool couldn't see here before, and the change now routes into the blindness. This
//     MR made something newly unverifiable; flag it loud.
//   - CARRIED BLIND: the change is resolved at its own level, but its effect surface
//     passes through a blind spot that ALREADY existed on this path. Not introduced
//     here — so it doesn't dominate — but disclosed, never background.
//   - ACCOUNTED: the forward cone is fully resolved, so the tool can show the COMPLETE
//     evidence (entrypoint cover, exact effect surface) for the reviewer to check.
//     This is NOT "approved": the tool vouches for STRUCTURE, not for correctness or
//     intent — the reviewer still verifies the resolved effects are the right ones. The
//     tool accepts nothing at face value; "accounted" only means "nothing here is
//     hidden from you."
//
// SCALE. On a large diff the renders collapse — but a collapse that drops the wrong
// node tells a confident lie, the exact failure goal (a) exists to prevent. So the
// collapse obeys the triage's own attention gradient, with an invariant: it proceeds
// only from the LOW-attention end (accounted, then carried) and NEVER collapses the
// new-blind zone or the boundary-effect surface; the accounted bulk rolls up BY PACKAGE
// (preserving which packages changed and the effects they touch), and every collapse is
// disclosed with a count — nothing ever vanishes silently. The full detail always
// remains in the markdown / --full render, so a diagram rollup hides nothing the
// reviewer cannot recover.
//
// This serves the two founding goals: (a) combat hallucination/context poisoning by
// being a deterministic reference frame whose incompleteness is LOUD — and, for a diff,
// whose NEWLY-incomplete regions are loudest; (b) route a reviewer's verification effort
// by confidence × novelty. Composition over the graph index and the impact evidence
// engine, a deterministic function of its inputs and no verdict — with an OPTIONAL policy
// that enables only the per-route write-movement section (reused from the review artifact).
//
// PROTOTYPE scope/limits (ride with the report): the changed set is the set-based
// node/edge/effect delta in BOTH directions (a function that gained OR lost a call/
// effect is in-scope, so a removed guard is not silently un-triaged); the
// per-function evidence is a static blast radius (what the
// change COULD touch, not the route a given input takes); novelty is computed by
// comparing each function's base vs branch FORWARD blind-spot set, so a brand-new call
// SITE to an already-reachable blind spot reads as carried, not new; and "accounted" is
// structural completeness, never approval.
package reviewtriage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/review"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
)

// defaultMaxNodes is the per-zone function-node budget before a zone collapses (the
// accounted zone rolls up by package; a blind zone caps with a disclosed "+N more").
// Effects and the new-blind detail are never bounded by it.
const defaultMaxNodes = 40

// Options tunes the renders for scale. The zero value is the default (collapse large
// zones at defaultMaxNodes); Full renders every node (the escape hatch).
type Options struct {
	MaxNodes int  // function-node budget per zone before collapse; 0 ⇒ defaultMaxNodes
	Full     bool // render every node, no collapse
}

func (o Options) budget() int {
	if o.MaxNodes > 0 {
		return o.MaxNodes
	}
	return defaultMaxNodes
}

// collapseAccounted reports whether the accounted zone should summarize by package:
// when the accounted count alone exceeds the budget (unless Full). It is the ONE rule
// both renders consult, so the markdown and the diagram never disagree on when the
// accounted zone is rolled up.
func (o Options) collapseAccounted(accounted int) bool {
	return !o.Full && accounted > o.budget()
}

// ChangedFn is one changed function with its evidence and the forward blind spots that
// keep the tool from fully accounting for it, split by whether THIS MR introduced them.
// Deterministic: every field derives from sorted graph data.
type ChangedFn struct {
	FQN  string `json:"fqn"`
	Tier int    `json:"tier,omitempty"`

	// Evidence — the facts a reviewer can check against the code.
	Entrypoints     []string `json:"entrypoints,omitempty"`       // reverse-reach cover: the routes it is live behind
	CoverUpperBound bool     `json:"cover_upper_bound,omitempty"` // the cover crossed a reverse HighFanOut seam — context, not a zone reason (#1)
	Effects         []string `json:"effects,omitempty"`           // forward boundary effects it can reach (human-readable)

	// NewSeams are serious forward blind spots this MR introduced or newly reaches;
	// CarriedSeams pre-existed on this path. The split separates the focus zone (new)
	// from the carried zone (disclosed, not new).
	NewSeams     []graph.BlindSpot `json:"new_seams,omitempty"`
	CarriedSeams []graph.BlindSpot `json:"carried_seams,omitempty"`
	BenignSeams  []string          `json:"benign_seams,omitempty"` // trivial-severity seams, set aside but disclosed (#2)

	NewOverApprox     bool `json:"new_over_approx,omitempty"`     // a forward HighFanOut over-approximation introduced by this MR
	CarriedOverApprox bool `json:"carried_over_approx,omitempty"` // a forward HighFanOut that pre-existed

	// Authored is true when this function's FQN was in the --scope-fqns set: the author
	// textually edited it (as opposed to the function merely changing STRUCTURALLY because
	// a callee moved). Only set when a scope set was supplied AND matched the graph;
	// omitempty so an unscoped report's JSON is byte-identical to before the field existed.
	// Disclosure + ranking input only — never a verdict.
	Authored bool `json:"authored,omitempty"`
}

// Report is the three-zone triage of an MR's changed functions, ordered by descending
// attention: new blindness, then carried blindness, then the fully-accounted rest. It also
// carries the verified "what this MR does" delta — the sound boundary-effect and entrypoint
// movement (a floor: the blind zones are where it is incomplete).
type Report struct {
	BaseNodes   int         `json:"base_nodes"`
	BranchNodes int         `json:"branch_nodes"`
	NewBlind    []ChangedFn `json:"new_blind,omitempty"`
	Carried     []ChangedFn `json:"carried,omitempty"`
	Accounted   []ChangedFn `json:"accounted,omitempty"`

	// The verified external-surface delta — what the MR does that the tool can vouch for,
	// derived from the boundary-edge and entrypoint sets (sound over statically-resolved
	// edges). Contract-level: an effect already present on another path is not "added".
	// Effects and entrypoints are both reported in BOTH directions (added and removed) so a
	// deleted route or dropped effect is never silently omitted.
	EffectsAdded       []string `json:"effects_added,omitempty"`
	EffectsRemoved     []string `json:"effects_removed,omitempty"`
	EntrypointsAdded   []string `json:"entrypoints_added,omitempty"`
	EntrypointsRemoved []string `json:"entrypoints_removed,omitempty"`

	// RouteIO is the per-route refinement of the write surface ("GET /x now writes Y"),
	// present only when Build was given a policy. Reuses fitness.RouteWrites.
	RouteIO []RouteMove `json:"route_io,omitempty"`

	// Scope fields — present only when BuildScoped was given a changed-FQN set (the
	// --scope-fqns input) that matched the graph. They carry the one signal the graph
	// cannot derive on its own: which functions the author TEXTUALLY edited, versus which
	// only changed structurally because a callee moved. All omitempty, so an unscoped
	// report serializes exactly as before.
	//
	// Scoped marks that scoping is ACTIVE (a set was supplied and matched ≥1 function), so
	// a render partitions author-edited blindness from callee-dragged-in blindness.
	// AuthoredScope echoes the matched author-edited functions present in the branch graph
	// (sorted) — the membership set a render tests seam SITES against. ScopeNote is a
	// fail-loud caution: a non-empty note means scoping fell back or matched nothing (an
	// FQN-format mismatch is surfaced, never silently swallowed).
	Scoped        bool     `json:"scoped,omitempty"`
	AuthoredScope []string `json:"authored_scope,omitempty"`
	ScopeNote     string   `json:"scope_note,omitempty"`
}

// RouteMove is one route (non-root entrypoint) whose verified WRITE surface changed
// base→branch — the per-route refinement of the service-level effect delta. Reuses
// fitness.RouteWrites for the attribution. Blind marks a count that stood on a blind
// frontier: the movement may be the model's knowledge shifting, not the code's behavior.
type RouteMove struct {
	Route   string   `json:"route"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Blind   bool     `json:"blind,omitempty"`
}

// TODO(prototype): deferred code-review findings — address when this graduates past the
// real-diff test, not before (premature hardening of code whose shape isn't proven):
//   - Build recomputes each changed function's forward cone 2-3x (ForNodes +
//     ForwardBlindSpots on branch, + ForwardBlindSpots on base); a single combined walk,
//     and restricting the base recompute to functions whose cone changed, would remove it.
//   - NewOverApprox/CarriedOverApprox are mutually-exclusive derived state (from
//     branchOver/baseOver); the "never both true" invariant is unguarded.
//   - changedFns re-implements review.diffGraphs' base→branch node/edge delta; nodeSet
//     duplicates review.nodeSet. Both could share one helper once the surface settles.

// Build computes the triage over the BRANCH graph (the post-merge reality the reviewer
// is judging). For each changed function it compares the branch forward blind-spot set
// against the BASE one to tell new blindness from carried. The blind zones are
// consequence-ranked (#4). A non-nil policy enables the per-route write-movement section
// (it is the input fitness.RouteWrites needs to enumerate routes and roots); pass nil to
// skip it (the service-level effect delta is still computed).
//
// Build is the unscoped form; BuildScoped layers on the author-changed-FQN signal.
func Build(base, branch *graph.Graph, p *policy.Policy) Report {
	return BuildScoped(base, branch, p, nil)
}

// BuildScoped is Build plus the one signal the graph cannot derive on its own: the set of
// FQNs the author TEXTUALLY edited (the --scope-fqns input). A function's structural change
// (it gained an out-edge because a callee moved) is not the same as the author editing it,
// and on an AI-scale diff the gap between the two is most of the noise. With a matching
// scope set the report marks each changed function Authored and a render partitions
// author-edited blindness from callee-dragged-in blindness.
//
// Fail-loud (CLAUDE.md tenet 2): a scope set that matches ZERO branch functions is almost
// certainly an FQN-format mismatch. Rather than silently empty the review list, BuildScoped
// records a ScopeNote caution and leaves scoping INACTIVE (the report is identical to the
// unscoped one but for the note). A nil/empty scope set is simply unscoped, no note.
func BuildScoped(base, branch *graph.Graph, p *policy.Policy, scope []string) Report {
	branchIx, baseIx := graph.NewIndex(branch), graph.NewIndex(base)
	baseNode := nodeSet(base)
	tier := tierLookup(branch)

	// Resolve the scope set against the branch graph. authored holds only FQNs that name a
	// real branch function (the membership set seam SITES are later tested against); a
	// supplied-but-unmatched set is the fail-loud case.
	authored, scopeNote := resolveScope(scope, nodeSet(branch))
	scoped := len(authored) > 0
	// Echo the matched authored set only when scoping is active, so an unscoped report's
	// AuthoredScope is nil (absent from JSON), not an empty slice.
	var authoredEcho []string
	if scoped {
		authoredEcho = setutil.SortedKeys(authored)
	}

	var newBlind, carried, accounted []ChangedFn
	for _, fqn := range changedFns(base, branch) {
		card := impact.ForNodes(branchIx, []string{fqn})                             // evidence: reverse cover + forward effects
		branchBlind, branchOver := impact.ForwardBlindSpots(branchIx, []string{fqn}) // forward-only (#1)
		branchSerious, benign := splitSeverity(branchBlind)                          // set aside benign seams (#2)

		// Base forward state for the SAME function — empty for a function new in this MR
		// (so all its blindness is, correctly, new).
		var baseSerious []graph.BlindSpot
		baseOver := false
		if baseNode[fqn] {
			pb, po := impact.ForwardBlindSpots(baseIx, []string{fqn})
			baseSerious, _ = splitSeverity(pb)
			baseOver = po
		}
		newSeams, carriedSeams := splitNewCarried(branchSerious, baseSerious)

		cf := ChangedFn{
			FQN:               fqn,
			Tier:              tier[fqn],
			Entrypoints:       card.Entrypoints,
			CoverUpperBound:   card.CoverOverApprox,
			Effects:           trimmedEffects(card.Effects),
			NewSeams:          newSeams,
			CarriedSeams:      carriedSeams,
			BenignSeams:       benignNotes(benign),
			NewOverApprox:     branchOver && !baseOver,
			CarriedOverApprox: branchOver && baseOver,
			Authored:          authored[fqn], // false (omitempty) when unscoped
		}
		switch {
		case len(cf.NewSeams) > 0 || cf.NewOverApprox:
			newBlind = append(newBlind, cf)
		case len(cf.CarriedSeams) > 0 || cf.CarriedOverApprox:
			carried = append(carried, cf)
		default:
			accounted = append(accounted, cf)
		}
	}
	sortByConsequence(newBlind)
	sortByConsequence(carried)

	// The verified "what this MR does" delta: boundary effects and entrypoints the branch
	// has that the base did not (and vice versa). Sound over statically-resolved edges.
	effAdded, effRemoved := stringSetDelta(boundaryEffectSet(base), boundaryEffectSet(branch))
	epsAdded, epsRemoved := stringSetDelta(entrypointSet(base), entrypointSet(branch))

	var routeIO []RouteMove
	if p != nil {
		routeIO = routeIODelta(p, baseIx, branchIx)
	}

	return Report{
		BaseNodes:          len(base.Nodes),
		BranchNodes:        len(branch.Nodes),
		NewBlind:           newBlind,
		Carried:            carried,
		Accounted:          accounted,
		EffectsAdded:       effAdded,
		EffectsRemoved:     effRemoved,
		EntrypointsAdded:   epsAdded,
		EntrypointsRemoved: epsRemoved,
		RouteIO:            routeIO,
		Scoped:             scoped,
		AuthoredScope:      authoredEcho,
		ScopeNote:          scopeNote,
	}
}

// resolveScope intersects the supplied author-edited FQN set with the branch's functions.
// It returns the matched set (the membership map a render tests function FQNs and seam
// sites against) and a fail-loud note. The three cases:
//   - nil/empty scope ⇒ unscoped: empty set, no note.
//   - supplied but ZERO matches ⇒ likely an FQN-format mismatch: empty set (scoping stays
//     inactive — the unscoped report is shown intact) AND a loud note, never a silent
//     empty review list.
//   - ≥1 match ⇒ the matched set (a partial match is fine: an authored FQN absent from the
//     graph is a deleted or body-only function the triage has nothing to say about).
//
// Determinism: the result is a set; callers echo it via setutil.SortedKeys.
func resolveScope(scope []string, branchNodes map[string]bool) (authored map[string]bool, note string) {
	if len(scope) == 0 {
		return nil, ""
	}
	authored = map[string]bool{}
	for _, fqn := range scope {
		if branchNodes[fqn] {
			authored[fqn] = true
		}
	}
	if len(authored) == 0 {
		return nil, fmt.Sprintf("scope: none of the %d supplied --scope-fqns matched any branch function (FQN-format mismatch?) — showing UNSCOPED, every change in attention scope", len(scope))
	}
	if len(authored) < len(scope) {
		note = fmt.Sprintf("scope: %d of %d supplied --scope-fqns matched the branch graph (the rest name no current function — deleted or body-only)", len(authored), len(scope))
	}
	return authored, note
}

// routeIODelta is the verified per-route write movement: which routes gained or lost a
// write base→branch. It is a thin mapper over review.RouteIODeltas — the ONE per-route
// delta (over fitness.RouteWrites), so this surface and the review artifact cannot
// disagree, and the rows arrive already sorted on the intrinsic route FQN (a display-name
// collision can never make the order arrival-dependent). Only the display-name resolution
// and the per-side blind collapse are local presentation.
func routeIODelta(p *policy.Policy, baseIx, branchIx *graph.Index) []RouteMove {
	names := routeNames(branchIx, baseIx) // branch name preferred (it describes branch behavior)
	display := func(fqn string) string {
		if n := names[fqn]; n != "" {
			return n
		}
		return fitness.ShortName(fqn)
	}
	// nil RW maps ⇒ review computes them via fitness.RouteWrites.
	rows := review.RouteIODeltas(p, baseIx, branchIx, nil, nil)
	out := make([]RouteMove, 0, len(rows))
	for _, rd := range rows {
		out = append(out, RouteMove{
			Route:   display(rd.Route),
			Added:   rd.Added,
			Removed: rd.Removed,
			Blind:   sideBlind(rd.Base) || sideBlind(rd.Branch),
		})
	}
	return out
}

func sideBlind(s *review.RouteIOSide) bool { return s != nil && s.Frontier == review.FrontierBlind }

// routeNames maps an entrypoint handler FQN to its human route name ("GET /x"), preferring
// the FIRST index's name; callers pass (branchIx, baseIx) so the branch name wins. Falls
// back per-FQN to nothing (the caller uses ShortName then).
func routeNames(ixs ...*graph.Index) map[string]string {
	m := map[string]string{}
	for _, ix := range ixs {
		for _, ep := range ix.Entrypoints() {
			if ep.Fn != "" && ep.Name != "" {
				if _, ok := m[ep.Fn]; !ok {
					m[ep.Fn] = ep.Name
				}
			}
		}
	}
	return m
}

// boundaryEffectSet is the set of human-readable boundary-effect labels the graph emits
// (the "boundary:" prefix trimmed, matching trimmedEffects), deduped — the contract-level
// I/O surface used for the verified delta.
func boundaryEffectSet(g *graph.Graph) map[string]bool {
	m := map[string]bool{}
	for _, e := range g.Edges {
		if e.IsBoundary() {
			m[strings.TrimPrefix(e.To, "boundary:")] = true
		}
	}
	return m
}

// entrypointSet is the set of named entrypoints (routes/consumers) the graph exposes.
func entrypointSet(g *graph.Graph) map[string]bool {
	m := map[string]bool{}
	for _, ep := range g.Entrypoints {
		if ep.Name != "" {
			m[ep.Name] = true
		}
	}
	return m
}

// stringSetDelta returns the sorted additions (in branch, not base) and removals (in base,
// not branch) — intrinsic order, no map iteration reaches the output (determinism).
func stringSetDelta(base, branch map[string]bool) (added, removed []string) {
	for k := range branch {
		if !base[k] {
			added = append(added, k)
		}
	}
	for k := range base {
		if !branch[k] {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// splitNewCarried partitions the branch's serious forward blind spots into those NOT in
// the base forward set (new) and those in both (carried). Seam identity is
// graph.BlindSpot.DedupKey — the SAME key impact dedups on, so a blind spot newly REACHED
// via an added edge (its site existed but was unreachable from this function in the base)
// is correctly new, and the two surfaces cannot drift on what a blind spot IS.
func splitNewCarried(branchSerious, baseSerious []graph.BlindSpot) (newSeams, carried []graph.BlindSpot) {
	had := map[string]bool{}
	for _, b := range baseSerious {
		had[b.DedupKey()] = true
	}
	for _, b := range branchSerious {
		if had[b.DedupKey()] {
			carried = append(carried, b)
		} else {
			newSeams = append(newSeams, b)
		}
	}
	return newSeams, carried
}

// changedFns is the sorted set of branch functions the MR structurally moved: new
// functions, signature changes, and functions that GAINED or LOST an outgoing call
// or effect. The lost-edge direction is load-bearing: a body change that REMOVED a
// call — e.g. deleting an auth-check invocation — leaves the function's signature
// and its surviving edges untouched, so an add-only delta would place it in NO
// triage zone and a reviewer would never be steered to the very edit that dropped a
// guard (M-28). An edge whose From was deleted entirely is not a surviving changed
// function, so lost edges are only attributed to a From still present in the branch.
func changedFns(base, branch *graph.Graph) []string {
	baseSig := make(map[string]string, len(base.Nodes))
	for _, n := range base.Nodes {
		baseSig[n.FQN] = n.Sig
	}
	branchNode := nodeSet(branch)
	changed := map[string]bool{}
	for _, n := range branch.Nodes {
		if old, existed := baseSig[n.FQN]; !existed || old != n.Sig {
			changed[n.FQN] = true
		}
	}
	baseEdge := make(map[string]bool, len(base.Edges))
	for _, e := range base.Edges {
		baseEdge[e.From+"\x00"+e.To] = true
	}
	branchEdge := make(map[string]bool, len(branch.Edges))
	for _, e := range branch.Edges {
		branchEdge[e.From+"\x00"+e.To] = true
	}
	for _, e := range branch.Edges {
		if branchNode[e.From] && !baseEdge[e.From+"\x00"+e.To] {
			changed[e.From] = true // gained an outgoing call/effect
		}
	}
	for _, e := range base.Edges {
		if branchNode[e.From] && !branchEdge[e.From+"\x00"+e.To] {
			changed[e.From] = true // lost an outgoing call/effect (still-present From)
		}
	}
	return setutil.SortedKeys(changed)
}

// splitSeverity divides forward-cone blind spots into the zone-worthy (serious) and the
// producer-tagged-benign (trivial). Only Severity=="trivial" is benign; every other value
// — including the empty default — is serious (#2 fails toward flagging, never hiding).
func splitSeverity(bs []graph.BlindSpot) (serious, benign []graph.BlindSpot) {
	for _, b := range bs {
		if b.Severity == "trivial" {
			benign = append(benign, b)
		} else {
			serious = append(serious, b)
		}
	}
	return serious, benign
}

// benignNotes renders the set-aside trivial seams, so an accounted change with a benign
// seam never claims a completeness it does not have.
func benignNotes(benign []graph.BlindSpot) []string {
	var out []string
	for _, b := range benign {
		site := b.Site
		if site == "" {
			site = "an undisclosed site"
		}
		out = append(out, fmt.Sprintf("%s at %s — producer-tagged trivial (a benign seam, e.g. a cancel-func dispatch)", b.Kind, site))
	}
	return out
}

// seamReasons renders blind spots as reviewer-actionable sentences.
func seamReasons(seams []graph.BlindSpot) []string {
	rs := make([]string, 0, len(seams))
	for _, b := range seams {
		rs = append(rs, blindReason(b))
	}
	return rs
}

// sortByConsequence orders a blind zone so scarce reviewer attention lands on the most
// consequential change first (#4): most-critical tier, then state-mutating, then blast
// radius, then FQN.
func sortByConsequence(fs []ChangedFn) {
	sort.SliceStable(fs, func(i, j int) bool {
		if a, b := tierRank(fs[i].Tier), tierRank(fs[j].Tier); a != b {
			return a < b
		}
		if a, b := reachesMutating(fs[i].Effects), reachesMutating(fs[j].Effects); a != b {
			return a
		}
		if a, b := len(fs[i].Entrypoints), len(fs[j].Entrypoints); a != b {
			return a > b
		}
		return fs[i].FQN < fs[j].FQN
	})
}

func tierRank(t int) int {
	if t <= 0 {
		return 1 << 30
	}
	return t
}

// reachesMutating is a RANKING-ONLY heuristic (no verdict rests on it): does the change's
// resolved effect surface include a write — a mutating SQL verb via the shared sqlverb
// source, or a bus PUBLISH? It matches on the verb TOKEN (not a substring), mirroring
// fitness.IsWrite's db/bus classification, so an effect whose name merely contains
// "PUBLISH" is not miscounted as a write.
func reachesMutating(effects []string) bool {
	for _, e := range effects {
		f := strings.Fields(e) // "db <OP> <table>" | "bus PUBLISH <event>" | "bus CONSUME <event>"
		if len(f) < 2 {
			continue
		}
		if f[0] == "db" && sqlverb.Mutating(f[1]) {
			return true
		}
		if f[0] == "bus" && f[1] == "PUBLISH" {
			return true
		}
	}
	return false
}

// blindReason renders one blind spot as a reviewer-actionable sentence.
func blindReason(b graph.BlindSpot) string {
	at := b.Site
	if at == "" {
		at = "an undisclosed site"
	}
	detail := ""
	if b.Detail != "" {
		detail = " (" + b.Detail + ")"
	}
	switch b.Kind {
	case "NonConstantBoundaryArg":
		return fmt.Sprintf("a boundary call with a NON-CONSTANT target at %s%s — the tool cannot tell which destination; verify the value", at, detail)
	case "UnresolvedDispatch", "UnresolvedCall":
		return fmt.Sprintf("a call through a function value the tool cannot resolve at %s%s — the actual callee, and what it does, is invisible here; verify it", at, detail)
	case "ConcurrentDispatch":
		return fmt.Sprintf("an unresolved goroutine dispatch at %s%s — concurrent behavior past this point is invisible to the tool", at, detail)
	case "DynamicEffect":
		return fmt.Sprintf("a DYNAMIC boundary effect at %s%s — the tool sees that an effect happens but not its full identity", at, detail)
	case "HighFanOut":
		return fmt.Sprintf("a dispatch site fanning to many possible targets at %s%s — the tool over-approximates here; confirm which target this change intends", at, detail)
	case "reflect":
		return fmt.Sprintf("reflection at %s%s — call structure here is invisible to static analysis", at, detail)
	case "unsafe", "cgo", "go:linkname":
		return fmt.Sprintf("%s at %s%s — bypasses the analyzable call graph", b.Kind, at, detail)
	case "ExternalBoundaryCall":
		return fmt.Sprintf("a call into a third-party package at %s%s — the tool cannot see inside it", at, detail)
	case "ImpeachmentSeam":
		return fmt.Sprintf("a behaviorally-proven blind spot at %s%s — runtime has shown this seam hides effects", at, detail)
	default:
		return fmt.Sprintf("%s at %s%s — the tool's view stops here", b.Kind, at, detail)
	}
}

// pkgRollup is a package's accounted changes summarized: the count and the union of the
// boundary effects they reach (the I/O surface is preserved even when functions collapse).
type pkgRollup struct {
	Pkg     string   `json:"pkg"`
	Count   int      `json:"count"`
	Effects []string `json:"effects,omitempty"`
}

// rollupAccounted groups accounted changes by package, deduping and sorting each group's
// effects — the scale collapse that keeps the I/O surface and package structure while
// shedding per-function detail. Deterministic (packages and effects sorted).
func rollupAccounted(fns []ChangedFn) []pkgRollup {
	count := map[string]int{}
	eff := map[string]map[string]bool{}
	for _, cf := range fns {
		p := fitness.PkgOf(cf.FQN)
		count[p]++
		if eff[p] == nil {
			eff[p] = map[string]bool{}
		}
		for _, e := range cf.Effects {
			eff[p][e] = true
		}
	}
	var out []pkgRollup
	for _, p := range setutil.SortedKeys(count) {
		out = append(out, pkgRollup{Pkg: p, Count: count[p], Effects: setutil.SortedKeys(eff[p])})
	}
	return out
}

// RenderMarkdown is the human-reviewer report: new blindness first, then carried, then
// the fully-accounted rest. On a large diff the accounted zone summarizes by package
// (unless Full) — the blind zones never collapse (silence is never a silent pass).
func (r Report) RenderMarkdown(o Options) string {
	var b strings.Builder
	n, c, a := len(r.NewBlind), len(r.Carried), len(r.Accounted)
	fmt.Fprintf(&b, "# MR review triage — where to spend your verification\n")
	fmt.Fprintf(&b, "_graph %d → %d nodes · %d changed function(s): %d NEW blind, %d carried blind, %d fully accounted_\n",
		r.BaseNodes, r.BranchNodes, n+c+a, n, c, a)

	if n+c+a == 0 {
		b.WriteString("\nNo structural change detected (body-only or no diff). The tool has nothing to triage here — that is not the same as \"safe\"; it means the change did not move the call graph, so verify behavior the usual way.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n## ⚠️  New blindness — %d change(s) this diff makes newly unverifiable (focus here)\n", n)
	if n > 0 {
		b.WriteString("_ordered by consequence: salience tier, then state-mutating, then blast radius_\n")
	} else {
		b.WriteString("_None — this diff introduces no new blind spot. (Pre-existing blindness, if any, is below.)_\n")
	}
	for _, cf := range r.NewBlind {
		fmt.Fprintf(&b, "\n### %s%s\n", cf.FQN, tierTag(cf.Tier))
		b.WriteString("This change makes new paths unverifiable — the tool could not see here before, and the change now routes into the blindness:\n")
		for _, reason := range seamReasons(cf.NewSeams) {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		if cf.NewOverApprox {
			b.WriteString("- the reachable-effect surface became an UPPER BOUND — the change's forward reach newly crosses a shared dispatch seam (HighFanOut)\n")
		}
		if len(cf.CarriedSeams) > 0 || cf.CarriedOverApprox {
			fmt.Fprintf(&b, "- (it also passes through pre-existing blindness: %s)\n", strings.Join(distinctKinds(cf.CarriedSeams), ", "))
		}
		writeEvidence(&b, cf, true)
	}

	fmt.Fprintf(&b, "\n## 🟡 Carried blindness — %d change(s) resolved here, but on an already-partly-blind path (disclosed, not new)\n", c)
	if c == 0 {
		b.WriteString("_None._\n")
	}
	for _, cf := range r.Carried {
		fmt.Fprintf(&b, "\n### %s%s\n", cf.FQN, tierTag(cf.Tier))
		b.WriteString("Resolved at its own level, but its effect surface passes through a blind spot that ALREADY existed — not introduced by this change. Flagged so it does not blend into the background:\n")
		for _, reason := range seamReasons(cf.CarriedSeams) {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		if cf.CarriedOverApprox {
			b.WriteString("- the reachable-effect surface is an UPPER BOUND through a pre-existing shared dispatch seam (HighFanOut)\n")
		}
		writeEvidence(&b, cf, true)
	}

	fmt.Fprintf(&b, "\n## ✅ Fully accounted — %d change(s): complete evidence shown\n", a)
	b.WriteString("_The tool can show the COMPLETE structural surface for these. That is not approval — the tool accepts nothing at face value; verify the resolved effects are the ones you intend._\n")
	if o.collapseAccounted(a) {
		fmt.Fprintf(&b, "_(summarized by package — %d changes over the %d-node budget; pass --full to expand each)_\n", a, o.budget())
		for _, rl := range rollupAccounted(r.Accounted) {
			effs := "no boundary effects"
			if len(rl.Effects) > 0 {
				effs = strings.Join(rl.Effects, ", ")
			}
			fmt.Fprintf(&b, "- **%s** — %d change(s); effects: %s\n", shortPkg(rl.Pkg), rl.Count, effs)
		}
	} else {
		for _, cf := range r.Accounted {
			fmt.Fprintf(&b, "\n### %s%s\n", cf.FQN, tierTag(cf.Tier))
			if len(cf.BenignSeams) == 0 {
				b.WriteString("Every path through this change is statically resolved — no dynamic dispatch, reflection, or opaque I/O on any reachable path. Evidence to verify against the code:\n")
			} else {
				b.WriteString("Statically resolved except a benign seam the producer tagged trivial; the effect surface is otherwise complete. Evidence to verify against the code:\n")
				for _, s := range cf.BenignSeams {
					fmt.Fprintf(&b, "- (set aside) %s\n", s)
				}
			}
			writeEvidence(&b, cf, false)
		}
	}

	b.WriteString("\n---\n")
	b.WriteString("_Triage is the static MAP (what each change COULD touch), not the route a given input takes; \"accounted\" is structural completeness, never approval. PROTOTYPE._\n")
	return b.String()
}

// writeEvidence prints the checkable facts of a change. partial marks a blind zone, where
// the facts are a FLOOR (a blind spot may hide more).
func writeEvidence(b *strings.Builder, cf ChangedFn, partial bool) {
	floor := ""
	if partial {
		floor = " (a FLOOR — the blind spot(s) above may hide more)"
	}
	coverNote := ""
	if cf.CoverUpperBound {
		coverNote = " ≤ (upper bound — reverse dispatch seam)"
	}
	if len(cf.Entrypoints) == 0 {
		fmt.Fprintf(b, "- live behind no discovered entrypoint%s\n", floor)
	} else {
		fmt.Fprintf(b, "- live behind %d entrypoint(s)%s%s:\n", len(cf.Entrypoints), coverNote, floor)
		for _, e := range cf.Entrypoints {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}
	switch {
	case len(cf.Effects) == 0 && !partial:
		b.WriteString("- reaches NO boundary effects — a pure/internal change (no DB, bus, or outbound I/O on any path)\n")
	case len(cf.Effects) == 0:
		b.WriteString("- no boundary effect resolved on the visible paths\n")
	default:
		surface := "the COMPLETE boundary-effect surface of this change"
		if partial {
			surface = "the boundary effects the tool CAN see"
		}
		fmt.Fprintf(b, "- reaches %d boundary effect(s) — %s%s:\n", len(cf.Effects), surface, floor)
		for _, e := range cf.Effects {
			fmt.Fprintf(b, "  - %s\n", e)
		}
	}
}

// summaryLine is one compact change row: `ShortName` [tier] ✍, the seam kinds (when in a
// blind zone), and the effect surface — all backtick-wrapped so special characters render
// literally in Markdown.
func summaryLine(cf ChangedFn, kinds []string) string {
	var sb strings.Builder
	sb.WriteString("`" + fitness.ShortName(cf.FQN) + "`")
	if cf.Tier > 0 {
		fmt.Fprintf(&sb, " [t%d]", cf.Tier)
	}
	if reachesMutating(cf.Effects) {
		sb.WriteString(" ✍")
	}
	if len(kinds) > 0 {
		sb.WriteString(" — " + backtickList(kinds, 0))
	}
	sb.WriteString(effSuffix(cf.Effects))
	return sb.String()
}

// effSuffix renders an effect set as a compact "→ a, b" suffix, or "" when there are none.
func effSuffix(effects []string) string {
	if len(effects) == 0 {
		return ""
	}
	return " → " + backtickList(effects, 0)
}

// backtickList joins items as `a`, `b`, … each in a backtick code span (so a <dynamic>
// label or other special char renders literally, not as stray Markdown/HTML). When max > 0
// and the list is longer, it shows max items then "…+N more" so a digest stays compact.
func backtickList(items []string, max int) string {
	shown, overflow := items, 0
	if max > 0 && len(items) > max {
		shown, overflow = items[:max], len(items)-max
	}
	q := make([]string, len(shown))
	for i, s := range shown {
		q[i] = "`" + s + "`"
	}
	out := strings.Join(q, ", ")
	if overflow > 0 {
		out += fmt.Sprintf(", …+%d more", overflow)
	}
	return out
}

// RenderMermaid draws the three-zone triage as a flowchart. On a large diff it collapses
// from the low-attention end only: a blind zone over budget caps with a disclosed
// "+N more" node (new-blind ordered by consequence, so the shown ones matter most), and
// the accounted bulk rolls up BY PACKAGE — each rollup node still wired to the effects it
// touches. The boundary-effect surface and the new-blind detail are never silently
// dropped; the full set is always in the markdown / --full render.
func (r Report) RenderMermaid(o Options) string {
	max := o.budget()
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	b.WriteString("  classDef newblind fill:#fde8e8,stroke:#e02424,color:#771d1d;\n")
	b.WriteString("  classDef carried fill:#fff4e5,stroke:#d97706,color:#7c4a03;\n")
	b.WriteString("  classDef accounted fill:#e6f4ea,stroke:#137333,color:#0b4a22;\n")
	b.WriteString("  classDef nseam fill:#fff8f0,stroke:#e02424,stroke-dasharray:4 3,color:#771d1d;\n")
	b.WriteString("  classDef cseam fill:#fffaf0,stroke:#d97706,stroke-dasharray:4 3,color:#7c4a03;\n")
	b.WriteString("  classDef rollup fill:#f0fdf4,stroke:#137333,stroke-dasharray:2 2,color:#0b4a22;\n")
	b.WriteString("  classDef effect fill:#eef2ff,stroke:#3b5bdb,color:#1e3a8a;\n")

	if len(r.NewBlind)+len(r.Carried)+len(r.Accounted) == 0 {
		b.WriteString("  none[\"No structural change to triage\"]\n")
		return b.String()
	}

	effID := map[string]string{}
	var effOrder []string
	effFor := func(label string) string {
		if id, ok := effID[label]; ok {
			return id
		}
		id := fmt.Sprintf("e%d", len(effOrder))
		effID[label] = id
		effOrder = append(effOrder, label)
		return id
	}
	type mmEdge struct{ from, to, style string }
	var edges []mmEdge

	// A blind zone: full nodes up to the budget, then a single disclosed overflow node
	// (never a silent drop). New-blind is consequence-ordered, so the shown ones matter
	// most; the overflow points the reviewer at the full markdown list.
	blindZone := func(title, prefix, nodeClass, seamClass string, fns []ChangedFn, seamsOf func(ChangedFn) []graph.BlindSpot) {
		shown, overflow := fns, 0
		if !o.Full && len(fns) > max {
			shown, overflow = fns[:max], len(fns)-max
		}
		fmt.Fprintf(&b, "  subgraph %s[\"%s\"]\n", strings.ToUpper(prefix), mmLabel(title))
		for i, cf := range shown {
			id := fmt.Sprintf("%s%d", prefix, i)
			fmt.Fprintf(&b, "    %s[\"%s\"]:::%s\n", id, mmLabel(nodeLabel(cf)), nodeClass)
			if kinds := distinctKinds(seamsOf(cf)); len(kinds) > 0 {
				sid := id + "s"
				fmt.Fprintf(&b, "    %s{{\"%s\"}}:::%s\n", sid, mmLabel("⚠ "+strings.Join(kinds, ", ")), seamClass)
				edges = append(edges, mmEdge{id, sid, "-.->"})
			}
			for _, eff := range cf.Effects {
				edges = append(edges, mmEdge{id, effFor(eff), "-.->"})
			}
		}
		if overflow > 0 {
			fmt.Fprintf(&b, "    %smore[\"+%d more — see report\"]:::%s\n", prefix, overflow, nodeClass)
		}
		b.WriteString("  end\n")
	}

	blindZone(fmt.Sprintf("⚠️ New blind — %d (focus)", len(r.NewBlind)), "n", "newblind", "nseam",
		r.NewBlind, func(cf ChangedFn) []graph.BlindSpot { return cf.NewSeams })
	blindZone(fmt.Sprintf("🟡 Carried blind — %d (not new)", len(r.Carried)), "c", "carried", "cseam",
		r.Carried, func(cf ChangedFn) []graph.BlindSpot { return cf.CarriedSeams })

	// Accounted: full nodes within budget, else rolled up by package — each rollup node
	// still wired to the effects its package touches (I/O surface kept). Same collapse rule
	// as the markdown (Options.collapseAccounted), so the two views never disagree.
	full := !o.collapseAccounted(len(r.Accounted))
	fmt.Fprintf(&b, "  subgraph ACCOUNTED[\"%s\"]\n", mmLabel(fmt.Sprintf("✅ Accounted — %d (complete evidence, not approval)", len(r.Accounted))))
	if full {
		for i, cf := range r.Accounted {
			id := fmt.Sprintf("a%d", i)
			fmt.Fprintf(&b, "    %s[\"%s\"]:::accounted\n", id, mmLabel(nodeLabel(cf)))
			for _, eff := range cf.Effects {
				edges = append(edges, mmEdge{id, effFor(eff), "-->"})
			}
		}
	} else {
		for i, rl := range rollupAccounted(r.Accounted) {
			id := fmt.Sprintf("a%d", i)
			fmt.Fprintf(&b, "    %s[\"%s\"]:::rollup\n", id, mmLabel(fmt.Sprintf("%s · %d accounted", shortPkg(rl.Pkg), rl.Count)))
			for _, eff := range rl.Effects {
				edges = append(edges, mmEdge{id, effFor(eff), "-->"})
			}
		}
	}
	b.WriteString("  end\n")

	for i, label := range effOrder {
		fmt.Fprintf(&b, "  e%d[[\"%s\"]]:::effect\n", i, mmLabel(label))
	}
	for _, e := range edges {
		fmt.Fprintf(&b, "  %s %s %s\n", e.from, e.style, e.to)
	}
	return b.String()
}

// nodeLabel is the compact function label: short name, tier badge, and a ✍ marker on a
// state-mutating change so the eye finds it.
func nodeLabel(cf ChangedFn) string {
	s := fitness.ShortName(cf.FQN)
	if cf.Tier > 0 {
		s += fmt.Sprintf(" [t%d]", cf.Tier)
	}
	if reachesMutating(cf.Effects) {
		s += " ✍"
	}
	return s
}

// distinctKinds is the sorted, deduped set of blind-spot kinds.
func distinctKinds(bs []graph.BlindSpot) []string {
	seen := map[string]bool{}
	for _, b := range bs {
		seen[b.Kind] = true
	}
	return setutil.SortedKeys(seen)
}

// mmLabel makes a string safe inside a Mermaid quoted label. It entity-escapes the full
// set of breakers — & (entity start), < > (the <dynamic> effect carries them), the quote
// that would close the label, and the apostrophe — and folds newlines/tabs to spaces.
// Mirrors graphio.labelEscaper (the producer-side Mermaid escaper); kept a local copy
// rather than crossing the flowmap/groundwork boundary for one presentation helper.
var mmReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
	"\n", " ",
	"\t", " ",
)

func mmLabel(s string) string { return mmReplacer.Replace(s) }

// shortPkg compacts a package import path to its last two segments for display.
func shortPkg(p string) string {
	if p == "" {
		return "(root)"
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

func tierTag(t int) string {
	if t <= 0 {
		return ""
	}
	return fmt.Sprintf("  [tier %d]", t)
}

func tierLookup(g *graph.Graph) map[string]int {
	m := make(map[string]int, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.FQN] = n.Tier
	}
	return m
}

func nodeSet(g *graph.Graph) map[string]bool {
	m := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		m[n.FQN] = true
	}
	return m
}

func trimmedEffects(effects []string) []string {
	out := make([]string, 0, len(effects))
	for _, e := range effects {
		out = append(out, strings.TrimPrefix(e, "boundary:"))
	}
	sort.Strings(out)
	return out
}
