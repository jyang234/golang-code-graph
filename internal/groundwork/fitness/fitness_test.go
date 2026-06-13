package fitness

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

const goldensDir = "../../../testdata/groundwork/goldens"

const (
	hGetUser      = "(*example.com/layeredsvc/internal/handler.Server).GetUser"
	hUpdateUser   = "(*example.com/layeredsvc/internal/handler.Server).UpdateUser"
	sSelectUser   = "(*example.com/layeredsvc/internal/store.Store).SelectUser"
	svcMain       = "example.com/layeredsvc.main"
	blindsvcStart = "(*example.com/blindsvc/internal/handler.Server).Create"
)

func loadGraph(t *testing.T, name string) *graph.Graph {
	t.Helper()
	g, err := graph.LoadFile(filepath.Join(goldensDir, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return g
}

// layeredPolicy is the strict handler→app→store policy, declared in code so a
// test can vary one field without editing the committed JSON.
func layeredPolicy() *policy.Policy {
	return &policy.Policy{
		Service: "layeredsvc", Version: 1,
		Layers: []policy.Layer{
			{Name: "handler", Packages: []string{"example.com/layeredsvc/internal/handler"}},
			{Name: "app", Packages: []string{"example.com/layeredsvc/internal/app"}},
			{Name: "store", Packages: []string{"example.com/layeredsvc/internal/store"}},
		},
		Layering: &policy.Layering{Roots: []string{"example.com/layeredsvc"}},
	}
}

func TestPkgOf(t *testing.T) {
	cases := map[string]string{
		hGetUser: "example.com/layeredsvc/internal/handler",
		"example.com/layeredsvc/internal/handler.writeJSON":         "example.com/layeredsvc/internal/handler",
		"example.com/layeredsvc.run":                                "example.com/layeredsvc",
		"example.com/layeredsvc.main":                               "example.com/layeredsvc",
		"(example.com/x/pkg.T).M":                                   "example.com/x/pkg",
		"example.com/x/gen.Decode[example.com/x/other.Application]": "example.com/x/gen",
	}
	for fqn, want := range cases {
		if got := PkgOf(fqn); got != want {
			t.Errorf("PkgOf(%q) = %q, want %q", fqn, got, want)
		}
	}
}

func TestLayeringCleanBase(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	res := Check(layeredPolicy(), graph.NewIndex(g))
	if !res.OK() {
		t.Fatalf("clean base should pass; got violations: %v", res.Violations())
	}
}

func TestLayeringSkipDetected(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	g.Edges = append(g.Edges, graph.Edge{From: hGetUser, To: sSelectUser, Tier: 2})
	res := Check(layeredPolicy(), graph.NewIndex(g))

	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "layering" {
		t.Fatalf("want 1 layering violation, got %v", v)
	}
	if v[0].From != hGetUser || v[0].To != sSelectUser {
		t.Errorf("violation names edge %s→%s, want %s→%s", v[0].From, v[0].To, hGetUser, sSelectUser)
	}
	if v[0].Summary != "handler → store skips 1 layer(s)" {
		t.Errorf("summary = %q", v[0].Summary)
	}
}

// A skip routed through an UNASSIGNED helper package (handler → codec → store)
// must be caught: the gate's core promise is that a layer cannot be skipped, and
// before the effective-edge logic this bounce evaded it entirely.
func TestLayeringSkipThroughUnassignedPackage(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	const codec = "example.com/layeredsvc/internal/codec.Decode" // not in any declared layer
	g.Nodes = append(g.Nodes, graph.Node{FQN: codec, Sig: "func()", Tier: 3})
	g.Edges = append(g.Edges,
		graph.Edge{From: hGetUser, To: codec, Tier: 3},
		graph.Edge{From: codec, To: sSelectUser, Tier: 2})

	res := Check(layeredPolicy(), graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "layering" {
		t.Fatalf("a skip bounced through an unassigned package must be caught; got %v", res.Findings)
	}
	if v[0].From != hGetUser || v[0].To != sSelectUser {
		t.Errorf("effective edge = %s → %s, want %s → %s", v[0].From, v[0].To, hGetUser, sSelectUser)
	}
}

// The legitimate spine handler → app → store must NOT be flagged: an intermediate
// layer (app) absorbs the descent, so handler does not "effectively" reach store.
func TestLayeringSpineThroughLayerAbsorbed(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json") // base IS handler → app → store
	if res := Check(layeredPolicy(), graph.NewIndex(g)); !res.OK() {
		t.Fatalf("the layered spine must pass; got %v", res.Violations())
	}
}

func TestLayeringUpwardDetected(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	// store calling back up into handler is an upward violation.
	g.Edges = append(g.Edges, graph.Edge{From: sSelectUser, To: hGetUser, Tier: 2})
	res := Check(layeredPolicy(), graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].Summary != "store → handler calls upward" {
		t.Fatalf("want one upward violation, got %v", v)
	}
}

func TestLayeringAllowListSuppresses(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	g.Edges = append(g.Edges, graph.Edge{From: hGetUser, To: sSelectUser, Tier: 2})
	p := layeredPolicy()
	p.Layering.Allow = []policy.Exception{{From: hGetUser, To: sSelectUser, Reason: "reviewed"}}
	res := Check(p, graph.NewIndex(g))
	if !res.OK() {
		t.Fatalf("allow-listed edge should be suppressed; got %v", res.Violations())
	}
}

func TestMustNotReachProvenAbsentIsSilent(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	p := layeredPolicy()
	p.MustNotReach = []policy.ReachRule{{
		Name: "read-only", From: []string{hGetUser},
		To: []string{"boundary:db INSERT", "boundary:db UPDATE", "boundary:db DELETE"},
	}}
	res := Check(p, graph.NewIndex(g))
	if len(res.Findings) != 0 {
		t.Fatalf("a proven-absent rule must be silent; got %v", res.Findings)
	}
}

func TestMustNotReachReachableViolation(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	p := layeredPolicy()
	p.MustNotReach = []policy.ReachRule{{
		Name: "update-must-not-write", From: []string{hUpdateUser},
		To: []string{"boundary:db UPDATE"},
	}}
	res := Check(p, graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "must_not_reach" {
		t.Fatalf("want a reachable violation, got %v", res.Findings)
	}
	if v[0].From != hUpdateUser || v[0].To != "boundary:db UPDATE users" {
		t.Errorf("violation = %s → %s", v[0].From, v[0].To)
	}
}

func TestMustNotReachNoPathFoundIsCaution(t *testing.T) {
	g := loadGraph(t, "blindsvc.graph.json")
	p := &policy.Policy{
		Service: "blindsvc", Version: 1,
		MustNotReach: []policy.ReachRule{{
			Name: "create-no-dynamic", From: []string{"(*example.com/blindsvc/internal/handler.Server).Create"},
			To: []string{"boundary:bus PUBLISH <dynamic>"},
		}},
	}
	res := Check(p, graph.NewIndex(g))
	if !res.OK() {
		t.Fatalf("a no-path-found caution must not fail the gate; got %v", res.Violations())
	}
	c := res.Cautions()
	if len(c) != 1 || c[0].Rule != "must_not_reach" {
		t.Fatalf("want one caution, got %v", res.Findings)
	}
}

func TestMustNotReachReachableThroughDynamic(t *testing.T) {
	g := loadGraph(t, "blindsvc.graph.json")
	p := &policy.Policy{
		Service: "blindsvc", Version: 1,
		MustNotReach: []policy.ReachRule{{
			Name: "publish-no-dynamic", From: []string{"(*example.com/blindsvc/internal/handler.Server).Publish"},
			To: []string{"boundary:bus PUBLISH <dynamic>"},
		}},
	}
	res := Check(p, graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].To != "boundary:bus PUBLISH <dynamic>" {
		t.Fatalf("want a reachable-through-dynamic violation, got %v", res.Findings)
	}
}

func TestIOBudget(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	ix := graph.NewIndex(g)

	// Budget of 2 passes: the UpdateUser route does exactly two writes.
	p := layeredPolicy()
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: 2}
	if res := Check(p, ix); !res.OK() {
		t.Fatalf("budget 2 should pass; got %v", res.Violations())
	}

	// Budget of 1 fails on the UpdateUser route.
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: 1}
	res := Check(p, ix)
	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "io_budget" || v[0].From != hUpdateUser {
		t.Fatalf("want one io_budget violation on the UpdateUser route, got %v", res.Findings)
	}
}

// A route whose DB mutations flow through a wrapper (non-constant SQL, labeled
// "db call") writes ZERO classified writes, so it trivially passes any budget —
// but that pass is not a proof the write surface is bounded. The budget check
// discloses it as a caution (advisory, exit 0), never a silent green (F1).
func TestIOBudgetCautionsOnUnclassifiedDB(t *testing.T) {
	const route = "(*example.com/svc/internal/handler.Server).CreateThing"
	const store = "(*example.com/svc/internal/store.Store).Insert"
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: store, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: store, Tier: 2},
			// The store mutates, but the labeler could not read the verb.
			{From: store, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 0}}
	res := Check(p, graph.NewIndex(g))

	if !res.OK() {
		t.Fatalf("an unclassified-DB caution must not fail the gate; got %v", res.Violations())
	}
	var found bool
	for _, c := range res.Cautions() {
		if c.Rule == "io_budget" && strings.Contains(c.Summary, "unenforceable") && strings.Contains(c.Summary, "db call") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an io_budget unenforceable caution naming `db call`, got %v", res.Findings)
	}
}

// A route whose forward cone touches a blind frontier (here a <dynamic> bus
// publish, the shape an oapi-codegen dispatch seam produces) has a write count
// that is only a lower bound — the budget discloses it as such, the half of F1
// that fires when forward reach is starved rather than mislabeled.
func TestIOBudgetCautionsOnBlindFrontier(t *testing.T) {
	const route = "(*example.com/svc/internal/handler.Server).Publish"
	const fan = "(*example.com/svc/internal/app.Bus).Fanout"
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: route, Sig: "func()", Tier: 1},
			{FQN: fan, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: route, To: fan, Tier: 2},
			{From: fan, To: "boundary:bus PUBLISH <dynamic>", Tier: 1, Boundary: "outbound-async"},
		},
	}
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 2}}
	res := Check(p, graph.NewIndex(g))
	if !res.OK() {
		t.Fatalf("a blind-frontier caution must not fail the gate; got %v", res.Violations())
	}
	var found bool
	for _, c := range res.Cautions() {
		if c.Rule == "io_budget" && strings.Contains(c.Summary, "lower bound") && strings.Contains(c.Summary, "blind") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an io_budget lower-bound caution naming the blind frontier, got %v", res.Findings)
	}
}

// A graph whose every DB write is classified (db INSERT/UPDATE/...) produces no
// unenforceable caution — the disclosure fires only where the labeler is blind.
func TestIOBudgetNoCautionWhenClassified(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	p := layeredPolicy()
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: 2}
	res := Check(p, graph.NewIndex(g))
	for _, c := range res.Cautions() {
		if c.Rule == "io_budget" {
			t.Fatalf("classified DB writes must not raise an io_budget caution; got %q", c.Summary)
		}
	}
}

// UnclassifiedDBLabel buckets only labels that MIGHT be an unproven write.
// Driver/transaction control (Ping*, Begin*, Commit, Rollback) and pool/session
// setters (Set*) reach the DB but cannot mutate a row, so they are excluded;
// "db call", ExecContext, and Query* stay in (a Postgres INSERT…RETURNING rides
// QueryContext, so a Query* might mutate). This is the R2 bucket narrowing.
func TestUnclassifiedDBLabelExcludesNonMutating(t *testing.T) {
	cases := map[string]bool{
		"boundary:db call":              true,  // opaque write path — kept
		"boundary:db ExecContext":       true,  // exec of non-constant SQL — kept
		"boundary:db QueryContext":      true,  // may be INSERT … RETURNING — kept
		"boundary:db QueryRowContext":   true,  // may be INSERT … RETURNING — kept
		"boundary:db PingContext":       false, // readiness probe — excluded
		"boundary:db BeginTx":           false, // transaction control — excluded
		"boundary:db Commit":            false, // transaction control — excluded
		"boundary:db Rollback":          false, // transaction control — excluded
		"boundary:db SetMaxOpenConns":   false, // pool config — excluded
		"boundary:db INSERT ledger":     false, // classified write, not unclassified
		"boundary:db SELECT applicants": false, // classified read, not unclassified
		"boundary:bus PUBLISH topic":    false, // not a DB effect
	}
	for to, want := range cases {
		if _, got := UnclassifiedDBLabel(graph.Edge{To: to, Boundary: "outbound-sync"}); got != want {
			t.Errorf("UnclassifiedDBLabel(%q) = %v, want %v", to, got, want)
		}
	}
}

// A service with a genuine opaque "db call" write route plus a health probe
// (db PingContext) and a read (db QueryRowContext) raises the unenforceable
// caution for exactly ONE route — the opaque write — not the probe or the read.
// Pre-R2 the probe and read were bucketed too, over-firing the caution 3×.
func TestIOBudgetUnenforceableExcludesProbesAndReads(t *testing.T) {
	const (
		writeRoute  = "(*example.com/svc/internal/inbound.Handler).Handle"
		writeStore  = "(*example.com/svc/internal/store.Store).Save"
		healthRoute = "(*example.com/svc/internal/server.Server).Healthcheck"
		readRoute   = "(*example.com/svc/internal/store.Store).GetEntity"
	)
	g := &graph.Graph{
		Nodes: []graph.Node{
			{FQN: writeRoute, Sig: "func()", Tier: 1},
			{FQN: writeStore, Sig: "func() error", Tier: 1},
			{FQN: healthRoute, Sig: "func()", Tier: 1},
			{FQN: readRoute, Sig: "func() error", Tier: 1},
		},
		Edges: []graph.Edge{
			{From: writeRoute, To: writeStore, Tier: 2},
			{From: writeStore, To: "boundary:db call", Tier: 1, Boundary: "outbound-sync"},
			{From: healthRoute, To: "boundary:db PingContext", Tier: 1, Boundary: "outbound-sync"},
			{From: readRoute, To: "boundary:db QueryRowContext", Tier: 1, Boundary: "outbound-sync"},
		},
	}
	p := &policy.Policy{Service: "svc", Version: 1, IOBudget: &policy.IOBudget{MaxWritesPerRoute: 1}}
	routes := RouteWrites(p, graph.NewIndex(g))

	hasUnclassified := func(route string) bool { return len(routes[route].Unclassified) > 0 }
	if !hasUnclassified(writeRoute) {
		t.Errorf("the opaque `db call` write route must remain unenforceable")
	}
	if hasUnclassified(healthRoute) {
		t.Errorf("a db PingContext health probe must not be flagged unenforceable: %v", routes[healthRoute].Unclassified)
	}
	// QueryRowContext stays in the bucket on purpose (INSERT … RETURNING rides it).
	if !hasUnclassified(readRoute) {
		t.Errorf("a db QueryRowContext route must stay in the bucket (may be INSERT … RETURNING)")
	}
}

// The composition root (main) is an entrypoint but not a route; its startup
// writes must not be charged against the per-route I/O budget.
func TestIOBudgetExcludesCompositionRoot(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	// main performs three startup writes — more than any route.
	for _, tbl := range []string{"a", "b", "c"} {
		g.Edges = append(g.Edges, graph.Edge{
			From: svcMain, To: "boundary:db INSERT " + tbl, Tier: 1, Boundary: "outbound-sync",
		})
	}
	p := layeredPolicy()
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: 2} // routes do ≤2; main does 3
	res := Check(p, graph.NewIndex(g))
	if !res.OK() {
		t.Fatalf("the composition root must be exempt from io_budget; got %v", res.Violations())
	}
}

// require_proof escalates an unprovable must_not_reach rule from caution to
// violation — fail-closed for high-stakes safety invariants.
func TestMustNotReachRequireProof(t *testing.T) {
	g := loadGraph(t, "blindsvc.graph.json")
	rule := policy.ReachRule{
		Name: "create-no-dynamic", From: []string{blindsvcStart},
		To: []string{"boundary:bus PUBLISH <dynamic>"},
	}

	// Default: an unprovable rule is a non-blocking caution.
	p := &policy.Policy{Service: "blindsvc", Version: 1, MustNotReach: []policy.ReachRule{rule}}
	if res := Check(p, graph.NewIndex(g)); !res.OK() {
		t.Fatalf("without require_proof the rule must remain a caution; got %v", res.Violations())
	}

	// With require_proof: the same unprovable rule blocks.
	rule.RequireProof = true
	p.MustNotReach = []policy.ReachRule{rule}
	res := Check(p, graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "must_not_reach" {
		t.Fatalf("require_proof must turn the unprovable rule into a violation; got %v", res.Findings)
	}
}

func TestIsWrite(t *testing.T) {
	cases := map[string]bool{
		"boundary:db SELECT users":          false,
		"boundary:db INSERT audit_log":      true,
		"boundary:db UPDATE users":          true,
		"boundary:bus PUBLISH user.created": true,
		"boundary:bus PUBLISH <dynamic>":    true,
		"boundary:bus CONSUME payment":      false,
		"boundary:peer GET /score/{id}":     false,
		"boundary:peer POST /charge/{id}":   true,
	}
	for to, want := range cases {
		if got := IsWrite(graph.Edge{To: to, Boundary: "outbound-sync"}); got != want {
			t.Errorf("IsWrite(%q) = %v, want %v", to, got, want)
		}
	}
}

func TestCheckDeterministic(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	g.Edges = append(g.Edges, graph.Edge{From: hGetUser, To: sSelectUser, Tier: 2})
	p := layeredPolicy()
	p.IOBudget = &policy.IOBudget{MaxWritesPerRoute: 1}
	a := Check(p, graph.NewIndex(g))
	b := Check(p, graph.NewIndex(g))
	if len(a.Findings) != len(b.Findings) {
		t.Fatalf("non-deterministic finding count: %d vs %d", len(a.Findings), len(b.Findings))
	}
	for i := range a.Findings {
		if a.Findings[i] != b.Findings[i] {
			t.Errorf("finding %d differs: %v vs %v", i, a.Findings[i], b.Findings[i])
		}
	}
}
