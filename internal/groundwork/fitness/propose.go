package fitness

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// Propose derives a baseline policy from a graph's measured facts — the
// cold-start answer. Everything it emits is a RATCHET OF CURRENT TRUTH, never
// an aspiration: layers from the package call DAG, waypoints that already
// guard every entrypoint-to-DB path, read-only invariants for routes that
// already write nothing, the write budget at today's maximum, current blind
// spots allow-listed observe-first. The tool proposes from facts; a
// CODEOWNER reviews, tightens, and commits — the trust model is unchanged.
//
// Self-verification is the load-bearing property: before returning, the
// proposal is checked against the very graph it was derived from, and
// anything the current code already violates is relaxed (a capped baseline
// allow-list) or withdrawn — and every such adjustment is reported in the
// guide as a LATENT FINDING, because "your inferred architecture is already
// violated here" is itself one of init's most valuable outputs.
func Propose(ix *graph.Index, service string) (*policy.Policy, string) {
	p := &policy.Policy{Service: service, Version: 1}
	var g guide
	g.intro(service)

	proposeLayers(ix, p, &g)
	proposeWaypoint(ix, p, &g)
	proposeReadOnly(ix, p, &g)
	proposeConcurrent(ix, p, &g)
	proposeBudget(ix, p, &g)
	proposeRatchet(ix, p, &g)

	reconcile(ix, p, &g)
	g.closing(ix)
	return p, g.String()
}

// proposeLayers ranks first-party packages by longest path from the
// entry-most packages in the package call DAG. A cycle anywhere withdraws the
// proposal — guessed layers on a cyclic codebase produce noise, not a ratchet.
func proposeLayers(ix *graph.Index, p *policy.Policy, g *guide) {
	rootPkg := ""
	for _, fqn := range ix.Nodes() {
		if strings.HasSuffix(fqn, ".main") {
			rootPkg = PkgOf(fqn)
		}
	}

	pkgEdges := map[string]map[string]bool{}
	pkgs := map[string]bool{}
	for _, e := range ix.Edges() {
		if e.IsBoundary() || !ix.Has(e.To) {
			continue
		}
		a, b := PkgOf(e.From), PkgOf(e.To)
		if a == "" || b == "" || a == rootPkg || b == rootPkg {
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
	roots := []string{}
	if rootPkg != "" {
		roots = append(roots, rootPkg)
	}
	p.Layering = &policy.Layering{Roots: roots}

	names := make([]string, len(p.Layers))
	for i, l := range p.Layers {
		names[i] = l.Name
	}
	g.section("Layers: proposed from the package call DAG",
		fmt.Sprintf("Order (top→bottom): **%s**, ranked by call depth; the composition root `%s` is exempt.\n\n"+
			"**Tighten by**: renaming the auto-named layers, and moving any package the inference mis-ranked — the layer map is YOUR claim, this is only the measured starting point.\n"+
			"**Delete if**: this service intentionally has no layered shape.", strings.Join(names, " → "), rootPkg))
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
	if len(writers) == 0 {
		g.section("Waypoint (must_pass_through): not proposed", "No DB write effects exist — nothing to guard yet. Revisit when the service writes.")
		return
	}
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
		To:      []string{"boundary:db INSERT", "boundary:db UPDATE", "boundary:db DELETE"},
		Through: []string{waypoint},
	}}
	g.section("Waypoint (must_pass_through): proposed",
		fmt.Sprintf("Every entrypoint-to-DB-write path already passes **`%s`** — ratcheted, so a future route that skips it fails the gate (the `entrypoint:*` selector binds brand-new handler packages automatically).\n\n"+
			"**Tighten by**: setting `\"require_proof\": true` if this seam is security-relevant (auth/tenancy) — unprovability then fails closed.\n"+
			"**Question for the team**: is this seam guarding by DESIGN or by accident? If by accident, decide whether to bless it or name the intended seam instead.", waypoint))
}

// proposeReadOnly ratchets every entrypoint that currently writes nothing:
// the read-route-stays-read-only invariant, derived rather than declared.
func proposeReadOnly(ix *graph.Index, p *policy.Policy, g *guide) {
	var readOnly []string
	for _, s := range ix.Sources() {
		if strings.HasSuffix(s, ".main") {
			continue
		}
		cone := append([]string{s}, ix.Reachable(s)...)
		writes := false
		for _, e := range ix.Effects(cone...) {
			if IsWrite(e) {
				writes = true
			}
		}
		if !writes {
			readOnly = append(readOnly, s)
		}
	}
	if len(readOnly) == 0 {
		g.section("Read-only routes (must_not_reach): not proposed", "Every entrypoint currently performs at least one external write — there are no read-only routes to ratchet.")
		return
	}
	p.MustNotReach = []policy.ReachRule{{
		Name: "read-routes-stay-read-only",
		From: readOnly,
		To:   []string{"boundary:db INSERT", "boundary:db UPDATE", "boundary:db DELETE", "boundary:bus PUBLISH"},
	}}
	g.section("Read-only routes (must_not_reach): proposed",
		fmt.Sprintf("%d entrypoint(s) currently reach no external write: %s. Ratcheted — a future change that makes a read route write fails the gate instead of shipping silently.\n\n"+
			"**Tighten by**: `\"require_proof\": true` on any of these that are unauthenticated.\n"+
			"**Delete entries** that are EXPECTED to start writing soon.", len(readOnly), shortList(readOnly)))
}

// proposeConcurrent ratchets the current truth about concurrency: if no
// concurrent path reaches a DB write today, lock that in.
func proposeConcurrent(ix *graph.Index, p *policy.Policy, g *guide) {
	seeds := map[string]bool{}
	for _, e := range ix.Edges() {
		if e.Concurrent && !e.IsBoundary() && ix.Has(e.To) {
			seeds[e.To] = true
		}
		if e.Concurrent && e.IsBoundary() && strings.HasPrefix(e.To, "boundary:db ") && IsWrite(e) {
			g.section("Concurrency (no_concurrent_reach): not proposed",
				"A concurrent DB write already exists — the rule would fire today. Decide whether that write is intended; if not, fix it and re-run init.")
			return
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
	}
	p.NoConcurrentReach = []policy.ConcurrentRule{{
		Name: "no-concurrent-db-writes",
		To:   []string{"boundary:db INSERT", "boundary:db UPDATE", "boundary:db DELETE"},
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
	for _, s := range ix.Sources() {
		if strings.HasSuffix(s, ".main") {
			continue
		}
		cone := append([]string{s}, ix.Reachable(s)...)
		n := 0
		for _, e := range ix.Effects(cone...) {
			if IsWrite(e) {
				n++
			}
		}
		if n > maxWrites {
			maxWrites = n
		}
	}
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: maxWrites}
	g.section("Write budget (io_budget): proposed",
		fmt.Sprintf("The busiest route currently reaches **%d** external write(s); the budget is set there — a side-effect blowout beyond today's maximum fails the gate.\n\n"+
			"**Tighten by**: lowering it after splitting the busiest route. Raising it later is a reviewed policy change, which is the point.", maxWrites))
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
				var kept []string
				for _, from := range p.MustNotReach[0].From {
					if from != f.From {
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
	g.section("What init CANNOT derive — the questions only the team can answer",
		"- **Path obligations** (`.flowmap.yaml`): lifecycle and ordering rules need intent — which acquire/release pairs and audit-before-publish orderings are REQUIRED, not just current. Review the graph's `effect_order` facts for committed effects that precede fallible calls, and decide which orderings are contracts.\n"+
			"- **Security-critical seams**: which `must_not_reach` / `must_pass_through` rules deserve `require_proof: true` (unprovability fails closed).\n"+
			"- **Intent vs accident**: every proposed rule encodes what the code DOES; only the team knows what it SHOULD do.\n\n"+
			"Next steps: `groundwork policy-check` the file, run `groundwork fitness` (it passes clean today by construction), put the policy under CODEOWNERS, wire `groundwork verify` into CI, and after a quiet week tighten the observe-first postures.")
}
