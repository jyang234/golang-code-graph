package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// The self-verification invariant: every proposed policy is a ratchet of
// current truth, so it must pass fitness with ZERO violations against the
// graph it was derived from — on every fixture, including the blind and
// obligation-bearing ones.
func TestProposeIsBaselineClean(t *testing.T) {
	for _, name := range []string{"layeredsvc", "blindsvc", "obligsvc", "loansvc"} {
		g := loadGraph(t, name+".graph.json")
		ix := graph.NewIndex(g)
		p, guide := Propose(ix, name)
		if err := p.Validate(); err != nil {
			t.Errorf("%s: proposed policy invalid: %v", name, err)
			continue
		}
		res := Check(p, ix)
		for _, f := range res.Violations() {
			// Graph-carried obligation verdicts are pre-existing facts init
			// must surface, not excuse; every POLICY-derived rule must be clean.
			if f.Rule != "obligation" {
				t.Errorf("%s: proposed rule violates its own source graph: %v", name, f)
			}
		}
		if !strings.Contains(guide, "questions only the team can answer") {
			t.Errorf("%s: guide missing the team-questions section", name)
		}
	}
}

// On layeredsvc the inference must rediscover the hand-written policy: the
// three layers in order, the app.Service waypoint, and the read-only route.
func TestProposeRediscoversLayeredsvc(t *testing.T) {
	ix := graph.NewIndex(loadGraph(t, "layeredsvc.graph.json"))
	p, guide := Propose(ix, "layeredsvc")

	names := []string{}
	for _, l := range p.Layers {
		names = append(names, l.Name)
	}
	if strings.Join(names, "→") != "handler→app→store" {
		t.Errorf("layers = %v, want handler→app→store", names)
	}
	if len(p.MustPassThrough) != 1 || p.MustPassThrough[0].Through[0] != "(*example.com/layeredsvc/internal/app.Service)" {
		t.Errorf("waypoint = %+v, want the app.Service seam", p.MustPassThrough)
	}
	if len(p.MustNotReach) != 1 || len(p.MustNotReach[0].From) != 1 || !strings.Contains(p.MustNotReach[0].From[0], "GetUser") {
		t.Errorf("read-only rule = %+v, want exactly GetUser", p.MustNotReach)
	}
	if p.IOBudget == nil || p.IOBudget.MaxWritesPerRoute != 2 {
		t.Errorf("budget = %+v, want max 2", p.IOBudget)
	}
	for _, want := range []string{"Tighten by", "entrypoint:*", "require_proof"} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide missing guidance marker %q", want)
		}
	}
}

// blindsvc: the current blind spots become the observe-first baseline.
func TestProposeRatchetsBlindSpots(t *testing.T) {
	g := loadGraph(t, "blindsvc.graph.json")
	p, _ := Propose(graph.NewIndex(g), "blindsvc")
	if p.BlindSpotRatchet == nil || p.BlindSpotRatchet.Gate || len(p.BlindSpotRatchet.Allow) != len(g.BlindSpots) {
		t.Errorf("ratchet = %+v, want observe-first with %d baseline allows", p.BlindSpotRatchet, len(g.BlindSpots))
	}
}

// R5: on a db-call substrate (production CRUD built from non-constant SQL, so the
// writes label "db call" and IsWrite cannot read them), init must NOT sweep the
// opaque write routes into `read-routes-stay-read-only`. The classified-only
// predicate used to call them read-only and propose a must_not_reach rule over
// the genuine write paths — silent-green. The fixture mirrors cgate: a consumer
// whose only DB effect is an opaque "db call" write, a health probe (db
// PingContext, provably non-mutating), and a route reaching no DB at all.
func TestProposeExcludesAndDisclosesDBCallWrites(t *testing.T) {
	const (
		writeRoute  = "(*example.com/svc/internal/inbound.Handler).Handle"
		writeStore  = "(*example.com/svc/internal/storage.PostgresStore).CreateMessage"
		healthRoute = "(*example.com/svc/internal/server.Server).Healthcheck"
		readRoute   = "(*example.com/svc/internal/server.Server).Livez"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: writeRoute, Sig: "func()", Tier: 1},
			{FQN: writeStore, Sig: "func() error", Tier: 1},
			{FQN: healthRoute, Sig: "func()", Tier: 1},
			{FQN: readRoute, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: writeRoute, To: writeStore, Tier: 2},
			// The consumer's whole write path is an opaque db call — zero classified writes.
			{From: writeStore, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
			// A readiness probe cannot mutate; it stays provably read-only.
			{From: healthRoute, To: "boundary:db PingContext", Tier: 1, Boundary: "outbound-sync"},
			// readRoute reaches no DB at all — the clean read-only baseline.
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")

	// The opaque write route must NOT be ratcheted as read-only...
	from := map[string]bool{}
	if len(p.MustNotReach) > 0 {
		for _, f := range p.MustNotReach[0].From {
			from[f] = true
		}
	}
	if from[writeRoute] {
		t.Errorf("the opaque db-call write route was swept into read-routes-stay-read-only: %v", p.MustNotReach[0].From)
	}
	// ...while the provably read-only routes (no DB, and a non-mutating probe) are.
	if !from[readRoute] || !from[healthRoute] {
		t.Errorf("provably read-only routes must still be ratcheted; From = %v", p.MustNotReach[0].From)
	}

	// ...and it must be DISCLOSED in the guide, in the R2 caution's voice.
	for _, want := range []string{"Read-only status unproven", "db call", ShortName(writeRoute)} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide missing unproven-route disclosure %q", want)
		}
	}
	// The budget section discloses its count is classified-only.
	if !strings.Contains(guide, "classified-only") {
		t.Errorf("guide budget section must disclose the count is classified-only")
	}
	// The "what init cannot derive" section names the opaque-write limit.
	if !strings.Contains(guide, "Opaque DB writes") {
		t.Errorf("guide closing must list opaque DB writes under what init cannot derive")
	}

	// The proposal must still be a clean ratchet of its own graph (no must_not_reach
	// rule fires, because the excluded opaque write never reaches a classified target).
	res := Check(p, graph.NewIndex(g))
	for _, f := range res.Violations() {
		if f.Rule != "obligation" {
			t.Errorf("proposed rule violates its own source graph: %v", f)
		}
	}
}

// R6: the R5 db-call disclosure reached proposeReadOnly/proposeBudget/
// proposeWaypoint but not proposeConcurrent. On a substrate where a concurrent
// (goroutine/defer-spawned) path reaches only an opaque "db call" write, bare
// IsWrite is blind to it, so init used to (a) print "No concurrent path reaches
// a DB write today" — false — and (b) ratchet `no-concurrent-db-writes` over
// the CLASSIFIED targets, a rule that guards nothing and fires the day the SQL
// becomes constant. The fixture extends the R5 storage service with one
// goroutine path performing the opaque write.
func TestProposeConcurrentExcludesAndDisclosesDBCallWrites(t *testing.T) {
	const (
		writeRoute = "(*example.com/svc/internal/inbound.Handler).Handle"
		spawned    = "(*example.com/svc/internal/worker.Worker).Persist"
		writeStore = "(*example.com/svc/internal/storage.PostgresStore).CreateMessage"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: writeRoute, Sig: "func()", Tier: 1},
			{FQN: spawned, Sig: "func()", Tier: 1},
			{FQN: writeStore, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			// The route spawns a goroutine that performs the write concurrently.
			{From: writeRoute, To: spawned, Tier: 2, Concurrent: true},
			{From: spawned, To: writeStore, Tier: 2},
			// The whole write path is an opaque db call — IsWrite reads nothing.
			{From: writeStore, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")

	// The vacuous-but-tripwiring rule must NOT be ratcheted over the opaque write.
	if p.NoConcurrentReach != nil {
		t.Errorf("a concurrent opaque write is unproven; want no no_concurrent_reach rule, got %+v", p.NoConcurrentReach)
	}
	// The false affirmative must be gone, replaced by the unproven disclosure.
	if strings.Contains(guide, "No concurrent path reaches a DB write today") {
		t.Errorf("guide falsely claims no concurrent DB write on a db-call substrate")
	}
	sec := guideSection(guide, "Concurrency (no_concurrent_reach): not proposed")
	if sec == "" {
		t.Fatalf("expected the concurrency not-proposed disclosure; guide:\n%s", guide)
	}
	for _, want := range []string{"UNPROVEN", "db call", "concurrent"} {
		if !strings.Contains(sec, want) {
			t.Errorf("concurrency disclosure missing %q; section:\n%s", want, sec)
		}
	}

	// Still a clean ratchet of its own graph (the rule was withdrawn, not violated).
	res := Check(p, graph.NewIndex(g))
	for _, f := range res.Violations() {
		if f.Rule != "obligation" {
			t.Errorf("proposed rule violates its own source graph: %v", f)
		}
	}
}

// The control: when the concurrent write is CLASSIFIED (literal SQL → db
// INSERT), proposeConcurrent sees it and reports the existing concurrent write
// — it must not be diverted into the unproven-disclosure path.
func TestProposeConcurrentClassifiedWriteStillDetected(t *testing.T) {
	const (
		route   = "(*example.com/svc/internal/inbound.Handler).Handle"
		spawned = "(*example.com/svc/internal/worker.Worker).Persist"
		store   = "(*example.com/svc/internal/storage.PostgresStore).CreateMessage"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: spawned, Sig: "func()", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: spawned, Tier: 2, Concurrent: true},
			{From: spawned, To: store, Tier: 2},
			{From: store, To: "boundary:db INSERT messages", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")
	if p.NoConcurrentReach != nil {
		t.Errorf("a classified concurrent write exists; want no rule, got %+v", p.NoConcurrentReach)
	}
	sec := guideSection(guide, "Concurrency (no_concurrent_reach): not proposed")
	if !strings.Contains(sec, "already reaches a DB write") {
		t.Errorf("classified concurrent write must report the existing write, not the unproven disclosure; section:\n%s", sec)
	}
	if strings.Contains(sec, "UNPROVEN") {
		t.Errorf("a classified write is proven, not unproven; section:\n%s", sec)
	}
}

// R6, direct-edge branch: a DIRECTLY concurrent opaque boundary edge
// (`go store.ExecRaw()` whose non-constant SQL labels boundary:db call). Its
// From is never added as a concurrent seed, so only the direct-edge scan can
// see it — distinct from the cone path the other R6 test exercises. Must be
// disclosed, not asserted away.
func TestProposeConcurrentDirectOpaqueBoundaryDisclosed(t *testing.T) {
	const route = "(*example.com/svc/internal/handler.Server).Fire"
	g := &graph.Graph{
		Nodes: []graph.Node{{FQN: route, Sig: "func()", Tier: 1}},
		Edges: []graph.Edge{
			{From: route, To: "boundary:db call", Tier: 1, Boundary: "outbound-async", Concurrent: true},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")
	if p.NoConcurrentReach != nil {
		t.Errorf("a directly concurrent opaque write is unproven; want no rule, got %+v", p.NoConcurrentReach)
	}
	if strings.Contains(guide, "No concurrent path reaches a DB write today") {
		t.Errorf("guide falsely claims no concurrent DB write on a directly concurrent db-call edge")
	}
	if sec := guideSection(guide, "Concurrency (no_concurrent_reach): not proposed"); !strings.Contains(sec, "UNPROVEN") {
		t.Errorf("direct concurrent opaque write must be disclosed; section:\n%s", sec)
	}
}

// R6, false-positive guard: a concurrent path reaching only db CONTROL ops
// (PingContext/BeginTx — provably non-mutating, UnclassifiedDBLabel returns
// false) must still propose the rule clean, NOT divert into the unproven
// disclosure. The opacity escape hatch must not swallow analyzable non-writes.
func TestProposeConcurrentControlOpsStillProposed(t *testing.T) {
	const (
		route   = "(*example.com/svc/internal/handler.Server).Do"
		spawned = "(*example.com/svc/internal/worker.Worker).Run"
	)
	for _, op := range []string{"PingContext", "BeginTx"} {
		g := &graph.Graph{
			Nodes: []graph.Node{
				{FQN: route, Sig: "func()", Tier: 1},
				{FQN: spawned, Sig: "func()", Tier: 1},
			},
			Edges: []graph.Edge{
				{From: route, To: spawned, Tier: 2, Concurrent: true},
				{From: spawned, To: "boundary:db " + op, Tier: 1, Boundary: "outbound-sync"},
			},
		}
		p, guide := Propose(graph.NewIndex(g), "svc")
		if p.NoConcurrentReach == nil {
			t.Errorf("%s is non-mutating; the rule must still be proposed", op)
		}
		if !strings.Contains(guide, "No concurrent path reaches a DB write today") {
			t.Errorf("%s: expected the clean proposed claim", op)
		}
		// worker.Run IS a concurrent first-party seed, so the note must NOT be the
		// vacuous variant — guards against a seed-miscount silently flipping it.
		if strings.Contains(guide, "Currently vacuous") {
			t.Errorf("%s: a concurrent seed exists; the proposed note must not be vacuous", op)
		}
	}
}

// R6 coverage: a concurrent path reaching only a classified READ (db SELECT) is
// provable — IsWrite=false and UnclassifiedDBLabel=false (SELECT is an excluded
// verb) — so the rule must be proposed clean, NOT diverted into the UNPROVEN
// disclosure. A regression dropping SELECT from the exclusion would surface here.
func TestProposeConcurrentSelectStillProposed(t *testing.T) {
	const (
		route   = "(*example.com/svc/internal/handler.Server).Do"
		spawned = "(*example.com/svc/internal/worker.Worker).Run"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: spawned, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: spawned, Tier: 2, Concurrent: true},
			{From: spawned, To: "boundary:db SELECT users", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")
	if p.NoConcurrentReach == nil {
		t.Errorf("a concurrent classified read must propose the rule clean; got nil")
	}
	if !strings.Contains(guide, "No concurrent path reaches a DB write today") {
		t.Errorf("expected the clean proposed claim for a concurrent SELECT")
	}
	if strings.Contains(guideSection(guide, "Concurrency (no_concurrent_reach)"), "UNPROVEN") {
		t.Errorf("a concurrent SELECT is provable, not UNPROVEN")
	}
}

// R6 coverage (direct-edge classified branch): a concurrent boundary edge that
// IS a classified write (`go store.Insert()`). Its From is never a seed, so only
// the first loop catches it, with a message ("A concurrent DB write already
// exists") distinct from the cone path — previously exercised by no test.
func TestProposeConcurrentDirectClassifiedWriteDetected(t *testing.T) {
	const route = "(*example.com/svc/internal/handler.Server).Fire"
	g := &graph.Graph{
		Nodes: []graph.Node{{FQN: route, Sig: "func()", Tier: 1}},
		Edges: []graph.Edge{
			{From: route, To: "boundary:db INSERT messages", Tier: 1, Boundary: "outbound-async", Concurrent: true},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")
	if p.NoConcurrentReach != nil {
		t.Errorf("a directly concurrent classified write exists; want no rule, got %+v", p.NoConcurrentReach)
	}
	sec := guideSection(guide, "Concurrency (no_concurrent_reach): not proposed")
	if !strings.Contains(sec, "A concurrent DB write already exists") {
		t.Errorf("direct classified concurrent write must report the existing-write message; section:\n%s", sec)
	}
	if strings.Contains(sec, "UNPROVEN") {
		t.Errorf("a classified write is proven, not unproven; section:\n%s", sec)
	}
}

// R6 coverage (mixed cone precedence): a concurrent cone reaching BOTH a
// classified write and an opaque label — the classified write must win (rule
// withheld via the existing-write message), regardless of effect iteration
// order. Pins the precedence the early-return at the write check provides, so a
// refactor that collected opaque labels ahead of that return cannot regress
// into surfacing UNPROVEN on a substrate that has a provable concurrent write.
func TestProposeConcurrentMixedConeClassifiedWins(t *testing.T) {
	const (
		route   = "(*example.com/svc/internal/handler.Server).Do"
		spawned = "(*example.com/svc/internal/worker.Worker).Run"
		store   = "(*example.com/svc/internal/storage.Store).Save"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: spawned, Sig: "func()", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: spawned, Tier: 2, Concurrent: true},
			{From: spawned, To: store, Tier: 2},
			{From: store, To: "boundary:db INSERT things", Tier: 1, Boundary: "outbound-sync"},
			{From: store, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")
	if p.NoConcurrentReach != nil {
		t.Errorf("a classified concurrent write exists; want no rule, got %+v", p.NoConcurrentReach)
	}
	sec := guideSection(guide, "Concurrency (no_concurrent_reach): not proposed")
	if !strings.Contains(sec, "already reaches a DB write") || strings.Contains(sec, "UNPROVEN") {
		t.Errorf("classified write must win over opaque on a mixed cone; section:\n%s", sec)
	}
}

// R6 follow-up (closing union): a goroutine spawned off a NON-route path (here,
// in main, which routeUnclassifiedDB skips) that performs an opaque write is
// flagged by the concurrency section but invisible to a route-scoped walk. The
// closing "Opaque DB writes" summary must union the concurrent cone so it does
// not under-report relative to the concurrency disclosure.
func TestProposeClosingUnionsConcurrentOpaqueWrites(t *testing.T) {
	const (
		mainFn = "example.com/svc/cmd/svc.main"
		reaper = "(*example.com/svc/internal/worker.Reaper).Run"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: mainFn, Sig: "func()", Tier: 1},
			{FQN: reaper, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			// main spawns a background goroutine that does an opaque write.
			{From: mainFn, To: reaper, Tier: 2, Concurrent: true},
			{From: reaper, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	_, guide := Propose(graph.NewIndex(g), "svc")

	// The concurrency section discloses the main-spawned opaque write...
	if csec := guideSection(guide, "Concurrency (no_concurrent_reach): not proposed"); !strings.Contains(csec, "UNPROVEN") {
		t.Errorf("concurrency section must disclose the main-spawned opaque write; section:\n%s", csec)
	}
	// ...and the closing summary must NOT omit it (the union fix). Pre-fix the
	// route-scoped walk skips main, so this label never reached the closing bullet.
	closing := guideSection(guide, "What init CANNOT derive")
	if !strings.Contains(closing, "Opaque DB writes") || !strings.Contains(closing, "db call") {
		t.Errorf("closing 'Opaque DB writes' must include the concurrent-cone opaque label; section:\n%s", closing)
	}
}

// R7: an oapi-codegen STRICT-SERVER topology. The generated
// `(*ServerInterfaceWrapper).Handler` method is a graph root whose static
// out-edges stop at the chi router BEFORE its own per-handler `$1` closure (the
// forward seam), so `Reachable(wrapper)` is starved and never sees the classified
// `db DELETE` the `$1` closure reaches. fitness, however, binds the
// `read-routes-stay-read-only` from-entry by NAME-EXPANSION (expandFroms/matchNodes),
// which pulls the `$1` closure in by prefix — so init used to propose the wrapper
// as read-only from the starved cone, then fail its own gate over the expanded
// family. proposeReadOnly must now judge over the same expansion the enforcer
// will, so the writing wrapper is excluded and the proposal stays self-clean.
//
// No existing fixture has this shape: loansvc/layeredsvc are direct-call (no `$1`
// closure), and the db-call fixtures are opaque-write, not classified-write behind
// a dispatch seam. This models the strict-server topology directly.
func TestProposeExcludesStrictServerWriterBehindSeam(t *testing.T) {
	const (
		wrapper = "(*example.com/svc/internal/api.ServerInterfaceWrapper).CreateEventTypeTemplate"
		// The generated handler closure: shares the wrapper's FQN as a prefix, so
		// expandFroms(wrapper) binds it — but it is NOT reachable from the wrapper.
		closure = "(*example.com/svc/internal/api.ServerInterfaceWrapper).CreateEventTypeTemplate$1"
		// The chi dispatch site that wires the generated handler closure: the
		// closure's single incoming edge comes from here, NOT from the wrapper.
		dispatch = "example.com/svc/internal/api.HandlerWithOptions$1"
		paramErr = "(*example.com/svc/internal/handler.Server).ParamErrorHandler"
		strictH  = "(*example.com/svc/internal/api.strictHandler).CreateEventTypeTemplate"
		store    = "(*example.com/svc/internal/storage.PostgresStore).deleteOutboxBySourceIDs"
		// A genuinely read-only route, to prove the rule is still proposed for the
		// routes that earn it.
		readRoute = "(*example.com/svc/internal/api.ServerInterfaceWrapper).GetHealth"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: wrapper, Sig: "func()", Tier: 1},
			{FQN: closure, Sig: "func()", Tier: 1},
			{FQN: dispatch, Sig: "func()", Tier: 1},
			{FQN: paramErr, Sig: "func()", Tier: 1},
			{FQN: strictH, Sig: "func() error", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
			{FQN: readRoute, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			// The wrapper root's static reach stops at the param error handler — it
			// never crosses chi's dynamic dispatch to its own `$1` closure (the seam),
			// so `Reachable(wrapper)` is starved and reaches no write.
			{From: wrapper, To: paramErr, Tier: 2},
			// The `$1` closure's single incoming edge comes from the chi dispatch
			// site, NOT the wrapper — so the wrapper cannot reach it, but
			// `expandFroms(wrapper)` still binds it by prefix.
			{From: dispatch, To: closure, Tier: 2},
			{From: closure, To: strictH, Tier: 2},
			{From: strictH, To: store, Tier: 2},
			// The classified write lives past the seam.
			{From: store, To: "boundary:db DELETE provisioning_outbox", Tier: 1, Boundary: "outbound-sync"},
			// GetHealth reaches no write at all — the clean read-only baseline.
		},
	}
	ix := graph.NewIndex(g)
	p, _ := Propose(ix, "svc")

	from := map[string]bool{}
	if len(p.MustNotReach) > 0 {
		for _, f := range p.MustNotReach[0].From {
			from[f] = true
		}
	}
	// The wrapper writes through its `$1` closure — it must NOT be ratcheted as a
	// read-only route, even though its bare forward cone is starved past the seam.
	if from[wrapper] {
		t.Errorf("the strict-server wrapper writes through its closure but was swept into read-routes-stay-read-only: From = %v", p.MustNotReach[0].From)
	}
	// The genuinely read-only route is still ratcheted.
	if !from[readRoute] {
		t.Errorf("the read-only route must still be ratcheted; From = %v", p.MustNotReach[0].From)
	}

	// The decisive assertion: the proposal must pass its own gate. Before the fix
	// fitness flagged the wrapper's `$1` closure reaching the DELETE — init's own
	// output exited 1 (R7). It must now be clean.
	res := Check(p, ix)
	for _, f := range res.Violations() {
		if f.Rule != "obligation" {
			t.Errorf("proposed policy violates its own source graph (R7): %v", f)
		}
	}
}

// R7 invariant (generic): for EVERY route the proposer keeps in
// `read-routes-stay-read-only`, the enforcer's own evalReach over that entry's
// name-expanded family must NOT be `reachable`. This is the property that makes
// init self-clean by construction — proven against the graph rather than asserted
// on one fixture. A regression that let proposeReadOnly judge over a node set
// narrower than expandFroms (the R7 bug) trips here on the strict-server topology.
func TestProposeKeptReadOnlyRoutesAreEnforcerClean(t *testing.T) {
	const (
		wrapper  = "(*example.com/svc/internal/api.W).Create"
		closure  = "(*example.com/svc/internal/api.W).Create$1"
		dispatch = "example.com/svc/internal/api.HandlerWithOptions$1"
		store    = "(*example.com/svc/internal/store.S).Del"
		health   = "(*example.com/svc/internal/api.W).Health"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: wrapper, Sig: "func()", Tier: 1},
			{FQN: closure, Sig: "func()", Tier: 1},
			{FQN: dispatch, Sig: "func()", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
			{FQN: health, Sig: "func()", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: dispatch, To: closure, Tier: 2},
			{From: closure, To: store, Tier: 2},
			{From: store, To: "boundary:db DELETE x", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	ix := graph.NewIndex(g)
	p, _ := Propose(ix, "svc")
	if len(p.MustNotReach) == 0 {
		t.Fatal("expected a read-only rule for the Health route")
	}
	to := p.MustNotReach[0].To
	for _, from := range p.MustNotReach[0].From {
		if v, ev := evalReach(ix, expandFroms(ix, []string{from}), to); v == reachable {
			t.Errorf("proposer KEPT %s read-only, but the enforcer finds its family reaches %s", from, ev.target)
		}
	}
}

// R7 soundness, prefix-sibling: a genuinely read-only route `GetUser` whose FQN
// is a PREFIX of a writing route `GetUserAvatar`. The enforcer's matchAny binds
// the longer name to a `From:[GetUser]` rule, so that rule cannot hold — init
// must therefore NOT ship it. The fix excludes GetUser (it fails safe by
// under-ratcheting), and the proposal stays self-clean. This pins that the
// prefix-matched enforcement semantics do not let init emit a rule the gate
// rejects, the same class of guarantee as the seam case.
func TestProposePrefixSiblingWriterStaysSelfClean(t *testing.T) {
	const (
		getUser   = "(*example.com/svc/internal/api.W).GetUser"
		getUserAv = "(*example.com/svc/internal/api.W).GetUserAvatar"
		avStore   = "(*example.com/svc/internal/store.S).Put"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: getUser, Sig: "func()", Tier: 1},
			{FQN: getUserAv, Sig: "func()", Tier: 1},
			{FQN: avStore, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: getUserAv, To: avStore, Tier: 2},
			{From: avStore, To: "boundary:db INSERT avatars", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	ix := graph.NewIndex(g)
	p, _ := Propose(ix, "svc")
	for _, from := range proposedFrom(p) {
		if from == getUser {
			t.Errorf("GetUser shares a name-prefix with the writer GetUserAvatar; the enforcer would bind the writer, so it must not be ratcheted")
		}
	}
	res := Check(p, ix)
	for _, f := range res.Violations() {
		if f.Rule != "obligation" {
			t.Errorf("proposed policy must be self-clean despite the prefix-sibling writer: %v", f)
		}
	}
}

// R7 coverage: the write behind the seam reaches a bus PUBLISH (a rule.To target
// that is NOT a db verb) through a SECOND closure ($2). Exercises both the
// multi-`$N`-closure family expansion and the bus-PUBLISH branch of the read-only
// target set — the main R7 fixture only covers a single db-DELETE closure.
func TestProposeExcludesBusPublisherAcrossMultiClosureSeam(t *testing.T) {
	const (
		wrapper  = "(*example.com/svc/internal/api.W).Emit"
		c1       = "(*example.com/svc/internal/api.W).Emit$1"
		c2       = "(*example.com/svc/internal/api.W).Emit$2"
		dispatch = "example.com/svc/internal/api.HandlerWithOptions$1"
		pub      = "(*example.com/svc/internal/bus.B).Send"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: wrapper, Sig: "func()", Tier: 1},
			{FQN: c1, Sig: "func()", Tier: 1},
			{FQN: c2, Sig: "func()", Tier: 1},
			{FQN: dispatch, Sig: "func()", Tier: 1},
			{FQN: pub, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: dispatch, To: c1, Tier: 2},
			{From: dispatch, To: c2, Tier: 2},
			{From: c2, To: pub, Tier: 2},
			{From: pub, To: "boundary:bus PUBLISH orders", Tier: 1, Boundary: "outbound-async"},
		},
	}
	ix := graph.NewIndex(g)
	p, _ := Propose(ix, "svc")
	for _, from := range proposedFrom(p) {
		if from == wrapper {
			t.Errorf("the wrapper publishes through its $2 closure but was ratcheted read-only")
		}
	}
	res := Check(p, ix)
	for _, f := range res.Violations() {
		if f.Rule != "obligation" {
			t.Errorf("proposed policy violates its own source graph: %v", f)
		}
	}
}

// proposedFrom returns the read-only rule's from-set, or nil if none was proposed.
func proposedFrom(p *policy.Policy) []string {
	if len(p.MustNotReach) == 0 {
		return nil
	}
	return p.MustNotReach[0].From
}

// guideSection returns the body of the first "## ..." section whose header
// contains titleSubstr, up to the next section header (or end of guide).
func guideSection(guide, titleSubstr string) string {
	for _, chunk := range strings.Split(guide, "\n## ") {
		if strings.Contains(chunk, titleSubstr) {
			return chunk
		}
	}
	return ""
}

// Regression: the unproven-routes disclosure must attribute only the opaque
// labels the UNPROVEN routes actually reach — not labels that belong to a
// classified-writer route's cone. Pre-fix, unclassLabels was collected from
// every non-main route, so a writer's `db ExecContext` inflated the count and
// label list of the read-only caution that names only the unproven routes.
func TestProposeUnprovenDisclosureScopedToNamedRoutes(t *testing.T) {
	const (
		writerRoute   = "(*example.com/svc/internal/handler.Server).Mixed"
		writerStore   = "(*example.com/svc/internal/store.Store).Both"
		unprovenRoute = "(*example.com/svc/internal/handler.Server).Sync"
		unprovenStore = "(*example.com/svc/internal/store.Store).Opaque"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: writerRoute, Sig: "func()", Tier: 1},
			{FQN: writerStore, Sig: "func() error", Tier: 1},
			{FQN: unprovenRoute, Sig: "func()", Tier: 1},
			{FQN: unprovenStore, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			// A classified writer that ALSO touches an opaque label (db ExecContext).
			{From: writerRoute, To: writerStore, Tier: 2},
			{From: writerStore, To: "boundary:db INSERT things", Tier: 1, Boundary: "outbound-sync"},
			{From: writerStore, To: "boundary:db ExecContext", Tier: 1, Boundary: "outbound-sync"},
			// The genuinely unproven route reaches a single opaque label (db call).
			{From: unprovenRoute, To: unprovenStore, Tier: 2},
			{From: unprovenStore, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	_, guide := Propose(graph.NewIndex(g), "svc")

	sec := guideSection(guide, "Read-only status unproven")
	if sec == "" {
		t.Fatalf("expected an unproven-routes disclosure section; guide:\n%s", guide)
	}
	// The named unproven route reaches exactly ONE opaque label; the writer's
	// db ExecContext must not be counted into or listed by this section.
	if !strings.Contains(sec, "1 DB effect label(s)") {
		t.Errorf("unproven disclosure must count only the unproven route's labels (1); section:\n%s", sec)
	}
	if strings.Contains(sec, "ExecContext") {
		t.Errorf("unproven disclosure must not list a classified-writer route's opaque label; section:\n%s", sec)
	}
	if !strings.Contains(sec, ShortName(unprovenRoute)) {
		t.Errorf("unproven disclosure must name the unproven route; section:\n%s", sec)
	}
}

// On a MIXED substrate (one classified write a seam guards, plus an opaque
// db-call write that bypasses it), the proposed waypoint must disclose that its
// "every path passes X" guarantee is classified-only — guardsAll cannot see the
// opaque write, so an absolute claim would be silent-green (R5, waypoint case).
func TestProposeWaypointDisclosesOpaqueBypass(t *testing.T) {
	const (
		guardedRoute = "(*example.com/svc/internal/handler.Server).Create"
		seam         = "(*example.com/svc/internal/app.Service)"
		seamMethod   = "(*example.com/svc/internal/app.Service).Do"
		store        = "(*example.com/svc/internal/store.Store).Insert"
		bypassRoute  = "(*example.com/svc/internal/handler.Server).Sync"
		bypassStore  = "(*example.com/svc/internal/store.Store).Upsert"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: guardedRoute, Sig: "func()", Tier: 1},
			{FQN: seamMethod, Sig: "func() error", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
			{FQN: bypassRoute, Sig: "func()", Tier: 1},
			{FQN: bypassStore, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			// Classified write path: route → seam → store → db INSERT (seam guards it).
			{From: guardedRoute, To: seamMethod, Tier: 2},
			{From: seamMethod, To: store, Tier: 2},
			{From: store, To: "boundary:db INSERT things", Tier: 1, Boundary: "outbound-sync"},
			// Opaque write path bypassing the seam entirely.
			{From: bypassRoute, To: bypassStore, Tier: 2},
			{From: bypassStore, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")

	if len(p.MustPassThrough) != 1 || p.MustPassThrough[0].Through[0] != seam {
		t.Fatalf("want a waypoint at the app.Service seam, got %+v", p.MustPassThrough)
	}
	if !strings.Contains(guide, "guarantee is classified-only") || !strings.Contains(guide, "db call") {
		t.Errorf("proposed waypoint must disclose the opaque-write bypass; guide:\n%s", guide)
	}
}

// When the ENTIRE write surface is opaque and no route is provably read-only,
// init proposes no read-only rule at all and says so — rather than asserting
// "every entrypoint writes" (false) or ratcheting an unsupported claim.
func TestProposeNoProvablyReadOnlyRoute(t *testing.T) {
	const (
		route = "(*example.com/svc/internal/inbound.Handler).Handle"
		store = "(*example.com/svc/internal/storage.PostgresStore).CreateMessage"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: store, Tier: 2},
			{From: store, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p, guide := Propose(graph.NewIndex(g), "svc")
	if p.MustNotReach != nil {
		t.Errorf("no route is provably read-only; want no must_not_reach, got %+v", p.MustNotReach)
	}
	if !strings.Contains(guide, "No entrypoint is PROVABLY read-only") {
		t.Errorf("guide must explain why no read-only rule was proposed")
	}
	// The waypoint section must not claim "No DB write effects exist".
	if strings.Contains(guide, "No DB write effects exist") {
		t.Errorf("waypoint section falsely claims no DB writes exist on a db-call substrate")
	}
}
