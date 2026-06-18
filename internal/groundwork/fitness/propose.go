package fitness

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// Propose derives a baseline policy from a graph's measured facts — the
// cold-start answer. Everything it emits is a RATCHET OF CURRENT TRUTH, never
// an aspiration: layers from the package call DAG, waypoints that already
// guard every entrypoint-to-DB path, read-only invariants for routes that
// already write nothing, the write budget at today's maximum, current blind
// spots allow-listed observe-first, and the current external write targets
// allow-listed observe-first (the effect ratchet). The tool proposes from
// facts; a CODEOWNER reviews, tightens, and commits — the trust model is unchanged.
//
// Self-verification is the load-bearing property: before returning, the
// proposal is checked against the very graph it was derived from, and
// anything the current code already violates is relaxed (a capped baseline
// allow-list) or withdrawn — and every such adjustment is reported in the
// guide as a LATENT FINDING, because "your inferred architecture is already
// violated here" is itself one of init's most valuable outputs.
func Propose(ix *graph.Index, service string) (*policy.Policy, string) {
	// Record the substrate the proposal was measured on, so a later gate can flag
	// a policy-vs-graph algorithm mismatch (the precision footgun) rather than
	// reporting its spurious findings as regressions.
	p := &policy.Policy{Service: service, Version: 1, Substrate: ix.Algo()}
	var g guide
	g.intro(service)

	proposeReclaimHint(ix, &g)
	proposeLayers(ix, p, &g)
	proposeWaypoint(ix, p, &g)
	proposeReadOnly(ix, p, &g)
	proposeConcurrent(ix, p, &g)
	proposeBudget(ix, p, &g)
	proposeRatchet(ix, p, &g)
	proposeEffectRatchet(ix, p, &g)

	reconcile(ix, p, &g)
	g.closing(ix)
	return p, g.String()
}

// proposeReclaimHint surfaces flowmap's existing strict-server reclaimer from the
// verdict path (issue 2). groundwork consumes a pre-built graph and cannot run the
// SSA-based reclaimer itself, so when it sees an UN-reclaimed dispatch seam at an
// HTTP route entry — an UnresolvedCall blind spot whose site IS a registered HTTP
// handler (the framework wired the per-route handler through an interface field the
// call-graph algorithm cannot resolve) — it recommends rebuilding the graph with
// `flowmap graph --reclaim`. The signal is STRUCTURAL (UnresolvedCall ∩ HTTP route
// handler), not a generated type name, so it covers the oapi-codegen strict-server
// shape and any framework with the same blind-at-the-route-entry topology. That
// seam blinds the dominant Go HTTP entry pattern: every route-anchored invariant is
// frontier-caveated at the endpoint entry, so a must_not_reach reads "no path found,
// but the frontier is blind" instead of a real proof. The reclaimer recovers the
// wrapper→handler edge soundly (R2: it only adds edges real execution can take).
// Skipped when the graph already carries reclaim provenance, so it stays quiet.
func proposeReclaimHint(ix *graph.Index, g *guide) {
	for _, e := range ix.Edges() {
		// A non-boundary edge carrying a reclaimer's provenance: already reclaimed.
		if e.Via != "" && !e.IsBoundary() {
			return
		}
	}
	httpRoute := map[string]bool{}
	for _, ep := range ix.Entrypoints() {
		if ep.Kind == "http" {
			httpRoute[ep.Fn] = true
		}
	}
	blind := 0
	for _, b := range ix.BlindSpots() {
		if b.Kind == string(blindspots.UnresolvedCall) && httpRoute[b.Site] {
			blind++
		}
	}
	if blind == 0 {
		return
	}
	g.section("⚠️ Un-reclaimed dispatch seam — HTTP route entries are blind",
		fmt.Sprintf("%d HTTP route entr(y/ies) are `UnresolvedCall` blind spots: the framework dispatches the per-route handler through an interface field wired at runtime (the oapi-codegen strict-server shape, and any framework like it), which the call-graph algorithm cannot resolve. Every route-anchored invariant is therefore frontier-caveated at the endpoint entry — a `must_not_reach` keyed on a route reports \"no path found, but the frontier is blind\" rather than a proof.\n\n"+
			"**Fix — rebuild the graph with `flowmap graph --reclaim`, then re-run init/fitness on it.** The strict-server reclaimer recovers the wrapper→handler edge: it is deterministic generated code with a fixed shape, so the edge is statically recoverable even though it is interface-dispatched, and adding it is SOUND (R2 — it only ever adds edges real execution can take, never a false proof of absence). This un-blinds the dominant Go HTTP entry pattern, making route-level invariants provable instead of frontier-caveated. The reclaimed graph carries `via: strict-server` provenance, disclosed on every verdict's substrate line, so a reclaim-informed verdict stays auditable.", blind))
}

// proposeLayers ranks first-party packages by longest path from the
// entry-most packages in the package call DAG. A cycle anywhere withdraws the
// proposal — guessed layers on a cyclic codebase produce noise, not a ratchet.
func proposeLayers(ix *graph.Index, p *policy.Policy, g *guide) {
	// A graph can span more than one binary (e.g. a server and a worker), each
	// with its own composition root. Collect EVERY .main's package, not just the
	// last one iterated — keying on the last (ix.Nodes() is sorted) silently
	// dropped all but the alphabetically-final root, exempting one binary's root
	// while ranking another's as an ordinary layer.
	rootPkgs := map[string]bool{}
	for _, fqn := range ix.Nodes() {
		if strings.HasSuffix(fqn, ".main") {
			if pkg := PkgOf(fqn); pkg != "" {
				rootPkgs[pkg] = true
			}
		}
	}

	pkgEdges := map[string]map[string]bool{}
	pkgs := map[string]bool{}
	for _, e := range ix.Edges() {
		if e.IsBoundary() || !ix.Has(e.To) {
			continue
		}
		a, b := PkgOf(e.From), PkgOf(e.To)
		if a == "" || b == "" || rootPkgs[a] || rootPkgs[b] {
			continue
		}
		pkgs[a], pkgs[b] = true, true
		if a != b {
			if pkgEdges[a] == nil {
				pkgEdges[a] = map[string]bool{}
			}
			pkgEdges[a][b] = true
		}
	}
	if len(pkgs) < 2 {
		g.section("Layers: not proposed", "Fewer than two first-party packages call each other — there is no layering to ratchet yet.")
		return
	}

	// Longest-path rank; cycle detection via DFS coloring.
	rank := map[string]int{}
	state := map[string]int{} // 0 unvisited, 1 in-stack, 2 done
	cyclic := false
	var visit func(pkg string) int
	visit = func(pkg string) int {
		if state[pkg] == 1 {
			cyclic = true
			return 0
		}
		if state[pkg] == 2 {
			return rank[pkg]
		}
		state[pkg] = 1
		depth := 0
		for _, next := range setutil.SortedKeys(pkgEdges[pkg]) {
			if d := visit(next) + 1; d > depth {
				depth = d
			}
		}
		state[pkg] = 2
		rank[pkg] = depth
		return depth
	}
	for _, pkg := range setutil.SortedKeys(pkgs) {
		visit(pkg)
	}
	if cyclic {
		g.section("Layers: not proposed (package cycle)",
			"The package call graph contains a cycle, so no layer ordering is a ratchet of current truth. Break the cycle (or accept that layering is not an invariant of this service yet), then re-run init.")
		return
	}

	// Depth-from-leaves → top layer has the greatest depth value; order desc.
	byRank := map[int][]string{}
	for pkg, r := range rank {
		byRank[r] = append(byRank[r], pkg)
	}
	var ranks []int
	for r := range byRank {
		ranks = append(ranks, r)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ranks)))
	if len(ranks) < 2 {
		g.section("Layers: not proposed", "All packages sit at one call depth — no descent order exists to ratchet.")
		return
	}
	for _, r := range ranks {
		members := byRank[r]
		sort.Strings(members)
		name := fmt.Sprintf("layer-%d", len(p.Layers)+1)
		if len(members) == 1 {
			if i := strings.LastIndexByte(members[0], '/'); i >= 0 {
				name = members[0][i+1:]
			} else {
				name = members[0]
			}
		}
		p.Layers = append(p.Layers, policy.Layer{Name: name, Packages: members})
	}
	roots := setutil.SortedKeys(rootPkgs)
	p.Layering = &policy.Layering{Roots: roots}

	names := make([]string, len(p.Layers))
	for i, l := range p.Layers {
		names[i] = l.Name
	}
	g.section("Layers: proposed from the package call DAG",
		fmt.Sprintf("Order (top→bottom): **%s**, ranked by call depth; the composition root(s) `%s` are exempt.\n\n"+
			"**Tighten by**: renaming the auto-named layers, and moving any package the inference mis-ranked — the layer map is YOUR claim, this is only the measured starting point.\n"+
			"**Delete if**: this service intentionally has no layered shape.", strings.Join(names, " → "), strings.Join(roots, ", ")))
}

// proposeWaypoint searches for a function (generalized to its receiver type
// when possible) that ALREADY guards every entrypoint-to-DB-write path, and
// ratchets it as must_pass_through.
func proposeWaypoint(ix *graph.Index, p *policy.Policy, g *guide) {
	writers := map[string]bool{}
	for _, e := range ix.Edges() {
		if strings.HasPrefix(e.To, "boundary:db ") && IsWrite(e) {
			writers[e.From] = true
		}
	}
	// Opaque DB effects scoped to routes (non-main entrypoints) — the same scope
	// proposeReadOnly/proposeBudget/closing use, so every section that discloses
	// the db-call frontier agrees on its count and labels.
	unclassified := routeUnclassifiedDB(ix)
	if len(writers) == 0 {
		// "No DB write effects exist" is a MEASUREMENT claim, and it is wrong on a
		// db-call substrate: a write whose SQL is non-constant labels "db call"
		// (or a bare method name), which IsWrite cannot read. Disclose those opaque
		// writes instead of asserting the service writes nothing (R5).
		if len(unclassified) > 0 {
			g.section("Waypoint (must_pass_through): not proposed",
				"No CLASSIFIED DB write exists, but there are "+dbCallPhrase(unclassified)+" — opaque writes may flow through them. A waypoint cannot be derived over writes the graph cannot see; if these routes mutate, name the intended seam by hand (or make the SQL constant so the verb becomes readable).")
		} else {
			g.section("Waypoint (must_pass_through): not proposed", "No DB write effects exist — nothing to guard yet. Revisit when the service writes.")
		}
		return
	}
	// proposeWaypoint judges write-reachability with guardedWalk over ix.Sources()
	// — the BARE source roots, not their name-expanded $N closure family. That is
	// the deliberate mirror of the enforcer this rule feeds, the same discipline
	// readOnlyCone documents for the read-only proposer: checkMustPassThrough binds
	// this rule's From (always policy.EntrypointSelector, set below) through
	// bindFroms→expandFroms, and expandFroms resolves entrypoint:* to EXACTLY
	// ix.Sources(). The entrypoint selector does NOT name-expand to $N closures —
	// only an explicit-FQN from-entry does (the read-only rule's case, which
	// readOnlyCone mirrors with expandFroms). So the proposer and the gate walk the
	// identical source set with the identical guardedWalk: a `through` that guardsAll
	// accepts is one the enforcer cannot find a bypass for, by construction.
	//
	// On an oapi strict-server topology the classified write behind the chi `$1`
	// dispatch seam is still reachable from the dispatch closure ROOT
	// (HandlerWithOptions$1, a graph source), which BOTH sides iterate — so neither
	// the proposer nor the enforcer is seam-starved here. Expanding each source to
	// its $1 family (the R7 readOnly fix) would make this proposer STRICTER than its
	// entrypoint:* enforcer rather than matching it, the opposite of self-clean.
	// Each proposer mirrors the binding of the enforcer it feeds; for the waypoint
	// rule that binding is the bare source set (R8). TestProposeWaypointStrictServer*
	// and TestProposeSelfCleanAcrossProposers pin this against the strict-server seam.
	sources := ix.Sources()

	guardsAll := func(through string) bool {
		reached := false
		for _, s := range sources {
			if matchAny(s, []string{through}) {
				continue
			}
			cone, _ := guardedWalk(ix, s, []string{through})
			for _, e := range ix.Effects(cone...) {
				if strings.HasPrefix(e.To, "boundary:db ") && IsWrite(e) {
					return false
				}
			}
			reached = true
		}
		return reached
	}

	var waypoint string
	for _, c := range ix.Nodes() {
		if writers[c] {
			continue // a writer trivially "guards" its own effect; not a seam
		}
		isSource := false
		for _, s := range sources {
			if s == c {
				isSource = true
			}
		}
		if isSource {
			continue
		}
		// Prefer the receiver-type generalization: it keeps guarding methods
		// added to the seam later.
		cand := c
		if i := strings.LastIndexByte(c, ')'); strings.HasPrefix(c, "(") && i > 0 {
			if typ := c[:i+1]; guardsAll(typ) {
				cand = typ
			}
		}
		if guardsAll(cand) {
			waypoint = cand
			break // Nodes() is sorted: deterministic choice
		}
	}
	if waypoint == "" {
		g.section("Waypoint (must_pass_through): not proposed",
			"No single function or type already guards every entrypoint-to-DB-write path. If one SHOULD (an auth or validation seam), that is a refactor target — add the rule after the seam exists.")
		return
	}
	p.MustPassThrough = []policy.PassRule{{
		Name:    "baseline-guards-db-writes",
		From:    []string{policy.EntrypointSelector},
		To:      dbWriteTargets(),
		Through: []string{waypoint},
	}}
	body := fmt.Sprintf("Every entrypoint-to-DB-write path already passes **`%s`** — ratcheted, so a future route that skips it fails the gate (the `entrypoint:*` selector binds brand-new handler packages automatically).\n\n"+
		"**Tighten by**: setting `\"require_proof\": true` if this seam is security-relevant (auth/tenancy) — unprovability then fails closed.\n"+
		"**Question for the team**: is this seam guarding by DESIGN or by accident? If by accident, decide whether to bless it or name the intended seam instead.", waypoint)
	// The seam guards every CLASSIFIED write path, but guardsAll cannot see opaque
	// writes (non-constant SQL labels "db call", which IsWrite skips). On a mixed
	// substrate the "every path" claim is classified-only — disclose so an opaque
	// write bypassing the seam does not read as guarded (R5, same class as above).
	if len(unclassified) > 0 {
		body += fmt.Sprintf("\n\n**Caution — guarantee is classified-only**: %s are NOT proven to pass this seam — an opaque write reaching the DB outside `%s` would not be caught. Make the SQL constant, or confirm the seam covers these paths by hand.",
			dbCallPhrase(unclassified), waypoint)
	}
	g.section("Waypoint (must_pass_through): proposed", body)
}

// dbWriteTargets is the canonical target list for every proposed rule that
// ratchets "no write reaches here" — one "boundary:db <VERB>" selector per verb
// in sqlverb.MutatingVerbs(), the SAME set IsWrite gates on (budget.go →
// sqlverb.Mutating). Deriving the list mechanically (rather than hand-typing the
// verbs) is what keeps the emitted rule from being strictly weaker than the
// check that built it: a verb added to sqlverb now reaches both IsWrite and the
// proposed target set at once, so a future concurrent/leaked write using it
// cannot slip the gate (the R6 regression). Returned fresh so callers can append
// (proposeReadOnly adds bus PUBLISH) without aliasing a shared backing array;
// TestDBWriteTargetsMatchSQLVerb guards the parity.
func dbWriteTargets() []string {
	verbs := sqlverb.MutatingVerbs()
	out := make([]string, len(verbs))
	for i, v := range verbs {
		out[i] = "boundary:db " + v
	}
	return out
}

// dbCallPhrase renders the canonical description of opaque DB effect labels —
// non-constant SQL the labeler cannot read as a write — with their count and
// sorted list. Every db-call disclosure (the four write-dependent proposers and
// the closing summary) opens with this exact phrase so the shared wording
// cannot drift between sites; each caller appends its own site-specific
// consequence (R6 follow-up — the consequences genuinely differ, the noun
// phrase must not).
func dbCallPhrase(labels []string) string {
	return fmt.Sprintf("%d DB effect label(s) built from non-constant SQL the labeler cannot read as a write (%s)",
		len(labels), strings.Join(labels, ", "))
}

// concurrentUnclassifiedDB returns the sorted distinct unclassified DB effect
// labels reachable from any CONCURRENT path — the direct concurrent boundary
// edges plus the cone of concurrent first-party seeds, the same frontier
// proposeConcurrent uses to decide whether "no concurrent DB write" is
// provable. The closing disclosure unions this with routeUnclassifiedDB so it
// cannot under-report an opaque write that only a goroutine spawned off a
// NON-route path (e.g. in main) reaches — which the concurrency section flags
// but a route-scoped walk never sees (R6 follow-up).
func concurrentUnclassifiedDB(ix *graph.Index) []string {
	set := map[string]bool{}
	seeds := map[string]bool{}
	for _, e := range ix.Edges() {
		if e.Concurrent && !e.IsBoundary() && ix.Has(e.To) {
			seeds[e.To] = true
		}
		// UnclassifiedDBLabel already requires a db boundary edge, so a
		// non-boundary concurrent seed edge is correctly ignored here.
		if e.Concurrent {
			if label, ok := UnclassifiedDBLabel(e); ok {
				set[label] = true
			}
		}
	}
	cone := setutil.SortedKeys(seeds)
	cone = append(cone, ix.Reachable(cone...)...)
	for _, e := range ix.Effects(cone...) {
		if label, ok := UnclassifiedDBLabel(e); ok {
			set[label] = true
		}
	}
	return setutil.SortedKeys(set)
}

// routeUnclassifiedDB returns the sorted distinct unclassified DB effect labels
// (non-constant SQL the labeler cannot read as a write — "db call" and friends)
// reachable from any ROUTE: a non-main entrypoint. Scoping to routes — not the
// whole graph (ix.Edges()) — keeps every section that discloses the db-call
// frontier in agreement, and makes the route-level "treated as possible writers,
// excluded from the read-only ratchet, uncounted in the write budget" framing
// accurate: a migration reachable only from main is not a route and is not what
// those sections exclude or count.
func routeUnclassifiedDB(ix *graph.Index) []string {
	set := map[string]bool{}
	for _, s := range ix.Sources() {
		if strings.HasSuffix(s, ".main") {
			continue
		}
		cone := append([]string{s}, ix.Reachable(s)...)
		for _, e := range ix.Effects(cone...) {
			if label, ok := UnclassifiedDBLabel(e); ok {
				set[label] = true
			}
		}
	}
	return setutil.SortedKeys(set)
}

// proposeReadOnly ratchets every entrypoint that currently writes nothing:
// the read-route-stays-read-only invariant, derived rather than declared.
//
// "Writes nothing" is a PROVABLE claim only when the classifier can read every
// DB verb the route reaches. A route whose only DB effects are unclassified
// ("db call" and friends — non-constant SQL IsWrite cannot read) MIGHT mutate:
// it is not provably read-only, so it is neither ratcheted nor counted as a
// read route. Ratcheting it would (a) assert a read-only claim the substrate
// can't support and (b) build an anti-protective rule — its `must_not_reach`
// target set is the CLASSIFIED writes, which an opaque write never reaches, so
// the rule guards nothing and instead fires the day someone makes the SQL
// constant (a strict analyzability improvement). Such routes are EXCLUDED and
// DISCLOSED, the same escape hatch the verify/fitness surfaces already give the
// db-call substrate (R5).
func proposeReadOnly(ix *graph.Index, p *policy.Policy, g *guide) {
	var readOnly []string
	var unproven []string
	unclassLabels := map[string]bool{}
	for _, s := range ix.Sources() {
		if strings.HasSuffix(s, ".main") {
			continue
		}
		cone := readOnlyCone(ix, s)
		writes := false
		routeUnclass := map[string]bool{}
		for _, e := range ix.Effects(cone...) {
			if IsWrite(e) {
				writes = true
			}
			if label, ok := UnclassifiedDBLabel(e); ok {
				routeUnclass[label] = true
			}
		}
		switch {
		case writes:
			// a classified writer — not a read route
		case len(routeUnclass) > 0:
			// Reaches a possible opaque write — record the labels THIS route
			// reaches so the disclosure attributes them to the routes it names.
			unproven = append(unproven, s)
			for l := range routeUnclass {
				unclassLabels[l] = true
			}
		default:
			readOnly = append(readOnly, s)
		}
	}

	// The forward-ratchet targets: the canonical write-effect labels plus bus
	// PUBLISH. If NONE of them bind anywhere in the graph (the service has zero
	// CLASSIFIED writes — typically because every write is non-constant SQL labeled
	// "db call"), the rule would bind nothing and fitness would correctly flag it
	// "to binds nothing — vacuous". Proposing it anyway would make init caution its
	// own baseline and frame a deliberate forward-ratchet as a user typo, so the
	// rule is DEFERRED with an honest disclosure instead (R-series self-clean: init
	// must not emit a baseline its own gate flags).
	to := append(dbWriteTargets(), "boundary:bus PUBLISH")
	switch {
	case len(readOnly) > 0 && bindsAnyTarget(ix, to):
		p.MustNotReach = []policy.ReachRule{{
			Name: "read-routes-stay-read-only",
			From: readOnly,
			To:   to,
		}}
		g.section("Read-only routes (must_not_reach): proposed",
			fmt.Sprintf("%d entrypoint(s) currently reach no external write: %s. Ratcheted — a future change that makes a read route write fails the gate instead of shipping silently.\n\n"+
				"**Tighten by**: `\"require_proof\": true` on any of these that are unauthenticated.\n"+
				"**Delete entries** that are EXPECTED to start writing soon.", len(readOnly), shortList(readOnly)))
	case len(readOnly) > 0:
		g.section("Read-only routes (must_not_reach): not proposed (no write target to forbid yet)",
			fmt.Sprintf("%d entrypoint(s) currently reach no external write, but the graph contains NO classified write target (no `boundary:db <verb>`, no `boundary:bus PUBLISH`) for a read-only ratchet to forbid — most often because every write is non-constant SQL the labeler reads as `db call`. A `read-routes-stay-read-only` rule would bind nothing and fitness would flag it vacuous, so it is deferred rather than shipped as a self-cautioning baseline.\n\n"+
				"**Unblock by**: making the write SQL constant (or building the graph with `flowmap graph --reclaim-sql`, which recovers the verb of constant-fragment SQL builders) so a real write target appears, then re-running init. Revisit also when the service starts writing.", len(readOnly)))
	case len(unproven) > 0:
		g.section("Read-only routes (must_not_reach): not proposed",
			"No entrypoint is PROVABLY read-only: every route that reaches no classified write also reaches a DB effect built from non-constant SQL the labeler cannot read as a write (see the caution below). Ratcheting any of them would assert a read-only claim the substrate cannot support.")
	default:
		g.section("Read-only routes (must_not_reach): not proposed", "Every entrypoint currently performs at least one external write — there are no read-only routes to ratchet.")
	}

	if len(unproven) > 0 {
		g.section("⚠️ Read-only status unproven — db-call routes excluded from the ratchet",
			fmt.Sprintf("%d route(s) reach no CLASSIFIED write but DO reach %s: %s.\n\n"+
				"Their read-only status is UNPROVEN, so they were left OUT of `read-routes-stay-read-only` rather than ratcheted on a claim the graph cannot support — these routes MIGHT mutate. Review before trusting; making the SQL constant exposes the verb and lets init classify it.",
				len(unproven), dbCallPhrase(setutil.SortedKeys(unclassLabels)), shortList(unproven)))
	}
}

// readOnlyCone returns the reachability cone proposeReadOnly judges a route source
// over, computed so the read-only verdict matches what fitness will ENFORCE.
//
// checkMustNotReach binds a from-entry through expandFroms (matchNodes/matchAny),
// which — beyond the source itself — binds the source's generated CLOSURE family,
// its `$N` closures, joined to the source FQN at the `$` identifier boundary.
// readOnlyCone reuses that SAME expandFroms primitive rather than re-deriving a
// private match, so the proposer and the gate expand a from-entry identically. On
// an oapi-codegen strict-server wrapper — a graph root whose static out-edges stop
// at the chi router before its own per-handler `$1` closure (the forward seam) —
// `Reachable(wrapper)` is starved and never sees the write the closure reaches,
// but `expandFroms(wrapper)` pulls the closure in, so the proposer excludes the
// wrapper instead of proposing a read-only entry the gate then violates (R7).
//
// reconcile re-runs the real Check on the assembled policy and prunes any entry
// that still fails, so reconcile — not this helper — is the AUTHORITATIVE guarantee
// that init's output passes its own gate; readOnlyCone is the cheaper up-front
// exclusion that keeps a correctly-excluded writer out of the noisy latent-findings
// list. This deliberately differs from proposeBudget / routeUnclassifiedDB, which
// judge the same sources over the BARE `ix.Reachable(s)` cone: those feed the
// io_budget rule, whose enforcer (RouteWrites) binds each route per-source WITHOUT
// expansion — so matching that enforcer means not expanding the closure family.
// Each proposer mirrors the binding of the enforcer it feeds.
func readOnlyCone(ix *graph.Index, source string) []string {
	family := expandFroms(ix, []string{source})
	return append(family, ix.Reachable(family...)...)
}

// proposeConcurrent ratchets the current truth about concurrency: if no
// concurrent path reaches a DB write today, lock that in.
//
// "No concurrent path reaches a DB write" is a PROVABLE claim only when the
// classifier can read every DB verb the concurrent cone reaches. A concurrent
// path whose DB effect is unclassified ("db call" and friends — non-constant
// SQL IsWrite cannot read) MIGHT be an unsupervised write: its absence is not
// proven, so asserting "no concurrent DB write" would be silent-green, and
// ratcheting `no-concurrent-db-writes` over the CLASSIFIED targets would build
// an anti-protective rule — the opaque write never reaches those targets, so
// the rule guards nothing and instead fires the day someone makes the SQL
// constant (a strict analyzability improvement → a new baseline violation).
// Such a substrate is left UNRATCHETED and DISCLOSED, mirroring the
// proposeReadOnly/proposeBudget treatment of the same db-call frontier (R6).
func proposeConcurrent(ix *graph.Index, p *policy.Policy, g *guide) {
	seeds := map[string]bool{}
	unclass := map[string]bool{}
	for _, e := range ix.Edges() {
		if e.Concurrent && !e.IsBoundary() && ix.Has(e.To) {
			seeds[e.To] = true
		}
		if e.Concurrent && e.IsBoundary() && strings.HasPrefix(e.To, "boundary:db ") {
			if IsWrite(e) {
				g.section("Concurrency (no_concurrent_reach): not proposed",
					"A concurrent DB write already exists — the rule would fire today. Decide whether that write is intended; if not, fix it and re-run init.")
				return
			}
			if label, ok := UnclassifiedDBLabel(e); ok {
				unclass[label] = true
			}
		}
	}
	cone := setutil.SortedKeys(seeds)
	cone = append(cone, ix.Reachable(cone...)...)
	for _, e := range ix.Effects(cone...) {
		if strings.HasPrefix(e.To, "boundary:db ") && IsWrite(e) {
			g.section("Concurrency (no_concurrent_reach): not proposed",
				"A goroutine/defer-spawned path already reaches a DB write. Decide whether it is intended; if not, fix it and re-run init.")
			return
		}
		if label, ok := UnclassifiedDBLabel(e); ok {
			unclass[label] = true
		}
	}
	// A concurrent path reaches an opaque DB label but no classified write: "no
	// concurrent DB write" is UNPROVEN here. Don't assert it, and don't ratchet a
	// rule that guards nothing today and fires the day the SQL becomes constant —
	// disclose instead, in the same voice as the read-only/io_budget cautions (R6).
	if len(unclass) > 0 {
		labels := setutil.SortedKeys(unclass)
		g.section("Concurrency (no_concurrent_reach): not proposed",
			"A concurrent (goroutine/defer-spawned) path reaches "+dbCallPhrase(labels)+" — it MIGHT be an unsupervised concurrent write. \"No concurrent path reaches a DB write\" is therefore UNPROVEN on this substrate, so no `no-concurrent-db-writes` rule was ratcheted: it would assert a claim the graph cannot support and would fire the day the SQL is made constant (a strict analyzability improvement). Make the SQL constant to expose the verb, or confirm by hand that no goroutine/defer path mutates.")
		return
	}
	// Like proposeReadOnly: if the graph has zero classified write targets, the
	// no-concurrent-db-writes rule binds nothing and fitness flags it vacuous (now
	// that no_concurrent_reach has the same to-binds-nothing check as must_not_reach).
	// Defer it rather than ship a baseline init's own gate would caution.
	if !bindsAnyTarget(ix, dbWriteTargets()) {
		g.section("Concurrency (no_concurrent_reach): not proposed (no write target to forbid yet)",
			"The graph contains no classified DB write target (`boundary:db <verb>`) for a `no-concurrent-db-writes` rule to forbid — most often because every write is non-constant SQL the labeler reads as `db call`. The rule would bind nothing and fitness would flag it vacuous, so it is deferred. Make the SQL constant (or build with `flowmap graph --reclaim-sql`) so a real write target appears, then re-run init; or revisit when the service writes.")
		return
	}
	p.NoConcurrentReach = []policy.ConcurrentRule{{
		Name: "no-concurrent-db-writes",
		To:   dbWriteTargets(),
	}}
	note := "No concurrent path reaches a DB write today — ratcheted, so a future \"make it async\" that introduces an unsupervised write fails the gate."
	if len(seeds) == 0 {
		note += " (Currently vacuous: this service spawns no concurrent first-party calls at all — the rule costs nothing and waits.)"
	}
	g.section("Concurrency (no_concurrent_reach): proposed", note)
}

// proposeBudget sets the per-route write budget at today's measured maximum.
func proposeBudget(ix *graph.Index, p *policy.Policy, g *guide) {
	maxWrites := 0
	unclassified := map[string]bool{}
	for _, s := range ix.Sources() {
		if strings.HasSuffix(s, ".main") {
			continue
		}
		cone := append([]string{s}, ix.Reachable(s)...)
		// Count DISTINCT write labels, exactly as the enforcer's RouteWrites does
		// (it compares len(routes[src].Writes), a set). Counting raw write edges
		// here would over-set the budget — three edges to the same target would
		// read as 3, letting the route later add two new distinct write targets
		// and still pass.
		writes := map[string]bool{}
		for _, e := range ix.Effects(cone...) {
			if label, ok := WriteLabel(e); ok {
				writes[label] = true
			}
			if label, ok := UnclassifiedDBLabel(e); ok {
				unclassified[label] = true
			}
		}
		if len(writes) > maxWrites {
			maxWrites = len(writes)
		}
	}
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: maxWrites}
	body := fmt.Sprintf("The busiest route currently reaches **%d** external write(s); the budget is set there — a side-effect blowout beyond today's maximum fails the gate.\n\n"+
		"**Tighten by**: lowering it after splitting the busiest route. Raising it later is a reviewed policy change, which is the point.", maxWrites)
	// The count is classified-only; an opaque "db call" mutation contributes zero.
	// Disclose so the budget number does not read as "writes are bounded" where the
	// labeler went blind on the verb — the same caution fitness raises at gate time.
	if len(unclassified) > 0 {
		body += "\n\n**Caution — this count is classified-only**: " + dbCallPhrase(setutil.SortedKeys(unclassified)) + "; they are NOT charged here, so a within-budget pass does not prove the write surface is bounded. `groundwork fitness` discloses the same on every run."
	}
	g.section("Write budget (io_budget): proposed", body)
}

// proposeRatchet allow-lists the blind spots that exist today, observe-first.
func proposeRatchet(ix *graph.Index, p *policy.Policy, g *guide) {
	r := &policy.BlindSpotRatchet{Gate: false}
	for _, b := range ix.BlindSpots() {
		r.Allow = append(r.Allow, policy.BlindSpotException{
			Kind: b.Kind, Site: b.Site, Reason: "baseline at init — review",
		})
	}
	p.BlindSpotRatchet = r
	g.section("Blind-spot ratchet: proposed (observe-first)",
		fmt.Sprintf("%d existing blind spot(s) are allow-listed as the baseline; NEW ones will be reported in every review from day one.\n\n"+
			"**Tighten by**: flipping `\"gate\": true` after a clean week — new dynamic dispatch then blocks the merge until reviewed. Each baseline entry deserves an eventual real reason or removal; `groundwork exceptions` will flag any that go dead.", len(r.Allow)))
}

// proposeEffectRatchet allow-lists the external write targets that exist today,
// observe-first — the sibling of proposeRatchet on the write surface (policy.go:
// "the sibling of BlindSpotRatchet with the same lifecycle"). Every CLASSIFIED
// write — a db mutation, a bus PUBLISH, a mutating outbound call — becomes a
// baseline EffectException, so a NEW write target is reported in every review from
// day one and a one-line `gate: true` flip makes it merge-blocking (the §10 first
// gate). It is ALWAYS proposed, even with an empty baseline: an empty allow-list
// arms the ratchet so the FIRST write on a read-only service is reported, the
// highest-signal case for that shape.
//
// The baseline is the exact set review's newWriteTargets diffs over — both label
// ix.Edges() through the single WriteLabel extractor — so init never reports its
// own write surface as new (the clean-baseline invariant, the effect-ratchet analog
// of "init passes its own gate"; guarded by the property test, since reconcile
// re-runs only Check and the effect ratchet is a review/Gate diff invisible to it).
//
// Opaque db-call writes (non-constant SQL) carry no readable verb, so they are NOT
// WriteLabels and this ratchet is structurally blind to them — only the non-gating
// db-label-drift count moves on a new one. Disclose that frontier rather than let
// "effect_ratchet: proposed" read as "the write surface is bounded" (self-honesty).
func proposeEffectRatchet(ix *graph.Index, p *policy.Policy, g *guide) {
	r := &policy.EffectRatchet{Gate: false}
	targets := map[string]bool{}
	unclassified := map[string]bool{}
	for _, e := range ix.Edges() {
		if label, ok := WriteLabel(e); ok {
			targets[label] = true
		}
		if label, ok := UnclassifiedDBLabel(e); ok {
			unclassified[label] = true
		}
	}
	for _, t := range setutil.SortedKeys(targets) {
		r.Allow = append(r.Allow, policy.EffectException{Target: t, Reason: "baseline at init — review"})
	}
	p.EffectRatchet = r

	// The dynamic-laundering coupling rides the main body (always): a new write
	// hidden behind dynamic dispatch collapses to an existing <dynamic> label and
	// escapes this ratchet's label diff — blind_spot_ratchet is its only backstop,
	// and §10's "gate effect first" is exactly the config where that backstop is
	// still off. One sentence names the dependency; the full mechanics live in the
	// EffectRatchet type doc.
	body := fmt.Sprintf("%d external write target(s) — db mutations, bus publishes, mutating outbound calls — are allow-listed as the baseline; a NEW write target (a new table, topic, or peer) is reported in every review from day one.\n\n"+
		"**Tighten by**: flipping `\"gate\": true` — a new write target then blocks the merge until reviewed. This is the §10 first gate, the highest-value catch (a seed/migration write reaching prod). A new write laundered through dynamic dispatch collapses to an existing `<dynamic>` label and is caught only by `blind_spot_ratchet` — gate both. `groundwork exceptions` flags any baseline entry that goes dead.", len(r.Allow))
	if len(unclassified) > 0 {
		body += "\n\n**Caution — opaque writes evade this ratchet**: " + dbCallPhrase(setutil.SortedKeys(unclassified)) + "; they are NOT classified writes, so a new opaque write is invisible here (only the non-gating db-label-drift count moves). Making the SQL constant exposes the verb and brings the write under the ratchet."
	}
	g.section("Write-target ratchet (effect_ratchet): proposed (observe-first)", body)
}

// reconcile is the self-verification: run the proposal against its own source
// graph and relax anything the current code already violates — recording each
// adjustment as a latent finding, which is signal, not noise.
func reconcile(ix *graph.Index, p *policy.Policy, g *guide) {
	const maxBaselineAllows = 10
	var latent []string
	for pass := 0; pass < 3; pass++ {
		res := Check(p, ix)
		v := res.Violations()
		if len(v) == 0 {
			break
		}
		for _, f := range v {
			latent = append(latent, fmt.Sprintf("`%s`: %s", f.Rule, f.Summary))
			switch f.Rule {
			case "layering":
				if p.Layering != nil && len(p.Layering.Allow) < maxBaselineAllows {
					p.Layering.Allow = append(p.Layering.Allow, policy.Exception{
						From: f.From, To: f.To, Reason: "baseline at init — latent violation, review",
					})
				} else {
					p.Layers, p.Layering = nil, nil
				}
			case "must_pass_through":
				if len(p.MustPassThrough) > 0 && len(p.MustPassThrough[0].Allow) < maxBaselineAllows {
					p.MustPassThrough[0].Allow = append(p.MustPassThrough[0].Allow, policy.Exception{
						From: f.From, To: f.To, Reason: "baseline at init — latent bypass, review",
					})
				} else {
					p.MustPassThrough = nil
				}
			case "must_not_reach":
				// Drop the from-entry that OWNS the violation. The finding names the
				// node that actually reached the target (f.From), which the enforcer
				// took from the NAME-EXPANDED from-set — for an oapi-codegen wrapper
				// that is the entry's `$1` closure, not the entry itself, so the old
				// bare `from == f.From` test never matched and the relaxation was a
				// silent no-op. Identify the owning entry through the SAME expandFroms
				// the enforcer used: its family contains the violating node iff it owns
				// the violation (exact match is included by expansion, so no separate
				// equality test is needed). The boundary-aware matchAny means this
				// binds only the true closure family, never a prefix-sibling route, so
				// the prune is precise. (readOnlyCone means init no longer proposes
				// such an entry; this keeps the relaxation honest if one reaches here.)
				kept := make([]string, 0, len(p.MustNotReach[0].From))
				for _, from := range p.MustNotReach[0].From {
					if !setutil.StringSet(expandFroms(ix, []string{from}))[f.From] {
						kept = append(kept, from)
					}
				}
				if len(kept) == 0 {
					p.MustNotReach = nil
				} else {
					p.MustNotReach[0].From = kept
				}
			case "no_concurrent_reach":
				p.NoConcurrentReach = nil
			case "io_budget":
				p.IOBudget.MaxWritesPerRoute++
			case "obligation":
				// Graph-carried verdicts: init cannot and must not excuse them
				// — they are pre-existing findings, surfaced in the guide.
			}
		}
	}
	// De-duplicate (obligation findings recur across reconcile passes).
	seen := map[string]bool{}
	uniq := latent[:0]
	for _, l := range latent {
		if !seen[l] {
			seen[l] = true
			uniq = append(uniq, l)
		}
	}
	latent = uniq
	if len(latent) > 0 {
		g.section("⚠️ Latent findings — the inference was violated by current code",
			"These rules were derivable from the code's dominant shape, but the code already breaks them somewhere. Each was relaxed with a `baseline at init` allow entry (or withdrawn) so the policy starts clean — **but every entry below is a real finding about today's code.** Fix the code and delete the entry, or bless it with a real reason:\n\n- "+strings.Join(latent, "\n- "))
	}
}

func shortList(fqns []string) string {
	short := make([]string, 0, len(fqns))
	for _, f := range fqns {
		short = append(short, "`"+ShortName(f)+"`")
	}
	if len(short) > 6 {
		return strings.Join(short[:6], ", ") + fmt.Sprintf(", … (%d total)", len(short))
	}
	return strings.Join(short, ", ")
}

// ---- the guide ---------------------------------------------------------------

type guide struct{ b strings.Builder }

func (g *guide) String() string { return g.b.String() }

func (g *guide) section(title, body string) {
	fmt.Fprintf(&g.b, "## %s\n\n%s\n\n", title, body)
}

func (g *guide) intro(service string) {
	fmt.Fprintf(&g.b, `# Proposed policy for %q — review guide

This policy was DERIVED from the service's measured graph, not designed:
every rule is a ratchet of what is already true, verified clean against the
graph it came from, with observe-first postures everywhere a posture exists.
**It is a proposal.** A CODEOWNER reviews and commits it; the agent helping
you refine it should treat each section's "tighten by / delete if / question
for the team" as its working checklist.

`, service)
}

func (g *guide) closing(ix *graph.Index) {
	cannot := "- **Path obligations** (`.flowmap.yaml`): lifecycle and ordering rules need intent — which acquire/release pairs and audit-before-publish orderings are REQUIRED, not just current. Review the graph's `effect_order` facts for committed effects that precede fallible calls, and decide which orderings are contracts.\n" +
		"- **Security-critical seams**: which `must_not_reach` / `must_pass_through` rules deserve `require_proof: true` (unprovability fails closed).\n" +
		"- **Intent vs accident**: every proposed rule encodes what the code DOES; only the team knows what it SHOULD do.\n"
	// db-call blindness is a MEASUREMENT limit, not an intent question, but it sits
	// here so the team sees it next to everything else init could not prove: the
	// write surface above is classified-only, and opaque writes are invisible to it.
	// This is the COMPLETE view — the union of the route-reachable frontier (what
	// the read-only and budget sections excluded/uncounted) and the concurrent-cone
	// frontier (what the concurrency section could not lock) — so the summary never
	// under-reports an opaque write some section above already flagged (R6).
	uset := map[string]bool{}
	for _, l := range routeUnclassifiedDB(ix) {
		uset[l] = true
	}
	for _, l := range concurrentUnclassifiedDB(ix) {
		uset[l] = true
	}
	unclassified := setutil.SortedKeys(uset)
	if len(unclassified) > 0 {
		cannot += "- **Opaque DB writes**: " + dbCallPhrase(unclassified) + ". init treats the routes and concurrent (goroutine/defer) paths reaching them as POSSIBLE writers — excluded from the read-only ratchet, uncounted in the write budget, and not lockable by the concurrency rule — but cannot tell whether they mutate. Make the SQL constant to expose the verb, or confirm the write surface by hand.\n"
	}
	g.section("What init CANNOT derive — the questions only the team can answer",
		cannot+"\nNext steps: `groundwork policy-check` the file, run `groundwork fitness` (it passes clean today by construction), put the policy under CODEOWNERS, wire `groundwork verify` into CI, and after a quiet week tighten the observe-first postures.")
}
