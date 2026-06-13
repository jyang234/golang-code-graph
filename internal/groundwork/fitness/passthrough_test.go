package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// must_pass_through (GX-1): every path from a From source to a To target must
// pass through a Through waypoint. In layeredsvc the app layer is the waypoint:
// every entrypoint-to-DB path legitimately runs handler → app.Service → store.

const (
	appService   = "(*example.com/layeredsvc/internal/app.Service)"
	hGetUserFast = "(*example.com/layeredsvc/internal/handler.Server).GetUserFast"
	v2Export     = "(*example.com/layeredsvc/internal/handlerv2.Server).Export"
)

func passPolicy(rule policy.PassRule) *policy.Policy {
	return &policy.Policy{Service: "layeredsvc", Version: 1, MustPassThrough: []policy.PassRule{rule}}
}

func appGuardsDB() policy.PassRule {
	return policy.PassRule{
		Name:    "app-guards-db",
		From:    []string{policy.EntrypointSelector},
		To:      []string{"boundary:db"},
		Through: []string{appService},
	}
}

func TestPassThroughGuardedBase(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	res := Check(passPolicy(appGuardsDB()), graph.NewIndex(g))
	if len(res.Findings) != 0 {
		t.Fatalf("clean base should be guarded (proven, no caution); got %v", res.Findings)
	}
}

func TestPassThroughBypassDetected(t *testing.T) {
	// The skip fixture wires a handler straight to the store — the same edge the
	// layering check catches, seen here as an unguarded path to the DB.
	g := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	res := Check(passPolicy(appGuardsDB()), graph.NewIndex(g))

	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "must_pass_through" {
		t.Fatalf("want 1 must_pass_through violation, got %v", res.Findings)
	}
	if v[0].From != hGetUserFast || v[0].To != "boundary:db SELECT users" {
		t.Errorf("violation = %s → %s, want %s → boundary:db SELECT users", v[0].From, v[0].To, hGetUserFast)
	}
	if !strings.Contains(v[0].Detail, "store.Store.SelectUser") || !strings.Contains(v[0].Detail, "boundary:db SELECT users") {
		t.Errorf("detail = %q, want the witness path through SelectUser to the effect", v[0].Detail)
	}
}

// The defining property of the entrypoint:* selector: a brand-new handler
// package with an unguarded route fires the rule with NO policy change. An
// FQN-glob From (naming the existing handler package) could not do this.
func TestPassThroughNewHandlerPackageStillBound(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	g.Nodes = append(g.Nodes, graph.Node{FQN: v2Export, Sig: "func()", Tier: 1})
	g.Edges = append(g.Edges, graph.Edge{From: v2Export, To: sSelectUser, Tier: 2})

	res := Check(passPolicy(appGuardsDB()), graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].From != v2Export {
		t.Fatalf("new handler package must be bound by entrypoint:*; got %v", res.Findings)
	}
}

func TestPassThroughAllowSuppressesExactPair(t *testing.T) {
	// Two bypasses; one allow-listed. Exactly the other must fire.
	g := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	g.Nodes = append(g.Nodes, graph.Node{FQN: v2Export, Sig: "func()", Tier: 1})
	g.Edges = append(g.Edges, graph.Edge{From: v2Export, To: sSelectUser, Tier: 2})

	rule := appGuardsDB()
	rule.Allow = []policy.Exception{{From: hGetUserFast, Reason: "read-only fast path, reviewed"}}
	res := Check(passPolicy(rule), graph.NewIndex(g))

	v := res.Violations()
	if len(v) != 1 || v[0].From != v2Export {
		t.Fatalf("allow must suppress exactly the listed pair; got %v", res.Findings)
	}
}

func TestPassThroughSourceIsWaypoint(t *testing.T) {
	// A source that itself matches Through is trivially guarded.
	g := loadGraph(t, "layeredsvc.graph.json")
	rule := policy.PassRule{
		Name:    "handler-is-its-own-guard",
		From:    []string{hGetUser},
		To:      []string{"boundary:db"},
		Through: []string{hGetUser},
	}
	res := Check(passPolicy(rule), graph.NewIndex(g))
	if len(res.Findings) != 0 {
		t.Fatalf("source matching through must be trivially guarded; got %v", res.Findings)
	}
}

func TestPassThroughBlindFrontierCaution(t *testing.T) {
	// blindsvc, from Publish: the bound target user.created is unreached from
	// Publish (only Create publishes it), but Publish's cone holds a reflect
	// site (encode.Marshal), so a hidden edge could skirt the waypoint —
	// "guarded" is unprovable. The To binds (Create publishes it), so this is
	// the genuine blind-frontier caution, not the unbindable-target case.
	g := loadGraph(t, "blindsvc.graph.json")
	rule := policy.PassRule{
		Name:    "publish-guarded",
		From:    []string{"(*example.com/blindsvc/internal/handler.Server).Publish"},
		To:      []string{"boundary:bus PUBLISH user.created"},
		Through: []string{"(*example.com/blindsvc/internal/notify.Notifier).Created"},
	}
	res := Check(passPolicy(rule), graph.NewIndex(g))
	c := res.Cautions()
	if len(res.Violations()) != 0 || len(c) != 1 {
		t.Fatalf("want exactly one caution on the blind frontier, got %v", res.Findings)
	}
	if !strings.Contains(c[0].Summary, "cannot prove every path is guarded") {
		t.Errorf("caution summary = %q", c[0].Summary)
	}

	rule.RequireProof = true
	res = Check(passPolicy(rule), graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "require_proof") {
		t.Fatalf("require_proof must escalate the blind frontier to a violation; got %v", res.Findings)
	}
}

// The unbindable-target fix on the must_pass_through side: a To matching no
// node and no effect anywhere (blindsvc has no DELETE) is disclosed as
// vacuous, not silently held — the field's cgate trust gap, mirrored.
func TestPassThroughUnbindableTo(t *testing.T) {
	g := loadGraph(t, "blindsvc.graph.json")
	rule := policy.PassRule{
		Name:    "nothing-deletes-unguarded",
		From:    []string{policy.EntrypointSelector},
		To:      []string{"boundary:db DELETE"},
		Through: []string{"example.com/blindsvc/internal/audit.Check"},
	}
	res := Check(passPolicy(rule), graph.NewIndex(g))
	c := res.Cautions()
	if len(res.Violations()) != 0 || len(c) != 1 || !strings.Contains(c[0].Summary, "to binds nothing") {
		t.Fatalf("an unbindable To must be a disclosed caution, got %v", res.Findings)
	}

	rule.RequireProof = true
	res = Check(passPolicy(rule), graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "require_proof") {
		t.Fatalf("require_proof must escalate an unbindable To to a violation, got %v", res.Findings)
	}
}

func TestPassThroughDeterministic(t *testing.T) {
	g := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	p := passPolicy(appGuardsDB())
	a, b := Check(p, graph.NewIndex(g)), Check(p, graph.NewIndex(g))
	if len(a.Findings) != len(b.Findings) {
		t.Fatalf("non-deterministic finding count")
	}
	for i := range a.Findings {
		if a.Findings[i] != b.Findings[i] {
			t.Fatalf("non-deterministic finding %d: %v vs %v", i, a.Findings[i], b.Findings[i])
		}
	}
}

// RF-1: entrypoint:* is one selector language for every From position. In a
// must_not_reach rule it expands to the graph sources instead of silently
// matching nothing (which read as proof of absence).
func TestEntrypointSelectorInMustNotReach(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	p := &policy.Policy{Service: "layeredsvc", Version: 1, MustNotReach: []policy.ReachRule{{
		Name: "no-entrypoint-reads",
		From: []string{policy.EntrypointSelector},
		To:   []string{"boundary:db SELECT"},
	}}}
	res := Check(p, graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 || v[0].Rule != "must_not_reach" {
		t.Fatalf("entrypoint:* must bind in must_not_reach (GetUser reaches SELECT users); got %v", res.Findings)
	}

	// And on a blind graph with require_proof, unprovability fails closed
	// instead of silently passing over an empty from-set.
	blind := loadGraph(t, "blindsvc.graph.json")
	p.MustNotReach[0].To = []string{"boundary:db DELETE"}
	p.MustNotReach[0].RequireProof = true
	res = Check(p, graph.NewIndex(blind))
	if v := res.Violations(); len(v) != 1 {
		t.Fatalf("require_proof over a blind frontier must fail closed; got %v", res.Findings)
	}
}
