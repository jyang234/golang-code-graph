package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

func concurrentPolicy(rule policy.ConcurrentRule) *policy.Policy {
	return &policy.Policy{Service: "layeredsvc", Version: 1, NoConcurrentReach: []policy.ConcurrentRule{rule}}
}

// A goroutine-spawned function whose cone hits a forbidden boundary fires; the
// same call on the synchronous path does not.
func TestConcurrentReachDetected(t *testing.T) {
	rule := policy.ConcurrentRule{Name: "no-async-writes", To: []string{"boundary:db UPDATE"}}

	// Synchronous base: UpdateUser writes via the normal path — no finding.
	g := loadGraph(t, "layeredsvc.graph.json")
	if res := Check(concurrentPolicy(rule), graph.NewIndex(g)); len(res.Findings) != 0 {
		t.Fatalf("synchronous writes must not fire the rule; got %v", res.Findings)
	}

	// Spawn the store call on a goroutine: the same write is now concurrent.
	g = loadGraph(t, "layeredsvc.graph.json")
	g.Edges = append(g.Edges, graph.Edge{
		From: "(*example.com/layeredsvc/internal/app.Service).UpdateProfile",
		To:   "(*example.com/layeredsvc/internal/store.Store).UpdateUser",
		Tier: 2, Concurrent: true,
	})
	res := Check(concurrentPolicy(rule), graph.NewIndex(g))
	v := res.Violations()
	if len(v) != 1 || v[0].Rule != "no_concurrent_reach" {
		t.Fatalf("want 1 no_concurrent_reach violation, got %v", res.Findings)
	}
	if v[0].To != "boundary:db UPDATE users" {
		t.Errorf("violation target = %q", v[0].To)
	}
}

// A concurrent boundary edge IS the target directly (`go publish(...)`).
func TestConcurrentDirectEffect(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	g.Edges = append(g.Edges, graph.Edge{
		From: "(*example.com/layeredsvc/internal/app.Service).UpdateProfile",
		To:   "boundary:bus PUBLISH user.updated", Tier: 1,
		Boundary: "outbound-async", Concurrent: true,
	})
	rule := policy.ConcurrentRule{Name: "no-async-publish", To: []string{"boundary:bus PUBLISH"}}
	res := Check(concurrentPolicy(rule), graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "concurrent path") {
		t.Fatalf("want the direct concurrent publish violation, got %v", res.Findings)
	}
}

// Issue 1a: a no_concurrent_reach rule whose To binds nothing is a dead selector
// (a typo'd or stale label), disclosed exactly like an unbindable must_not_reach
// target — a Caution by default, escalated under require_proof — never a silent
// "enforced" pass. This is the parity the issue asked for.
func TestConcurrentUnbindableTargetIsDisclosed(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	rule := policy.ConcurrentRule{Name: "no-async-zzz", To: []string{"boundary:db ZZZ_NONEXISTENT"}}
	res := Check(concurrentPolicy(rule), graph.NewIndex(g))
	c := res.Cautions()
	if len(res.Violations()) != 0 || len(c) != 1 || !strings.Contains(c[0].Summary, "to binds nothing") {
		t.Fatalf("an unbindable no_concurrent_reach To must be a disclosed caution, got %v", res.Findings)
	}

	rule.RequireProof = true
	res = Check(concurrentPolicy(rule), graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "require_proof") {
		t.Fatalf("require_proof must escalate an unbindable concurrent target, got %v", res.Findings)
	}
}

// No concurrent path over a blind frontier: caution, escalated by require_proof.
func TestConcurrentBlindFrontier(t *testing.T) {
	g := loadGraph(t, "blindsvc.graph.json")
	// Spawn something concurrent so the cone exists and crosses blind territory.
	var anyNode string
	for _, n := range g.Nodes {
		if strings.Contains(n.FQN, "handler") {
			anyNode = n.FQN
			break
		}
	}
	g.Edges = append(g.Edges, graph.Edge{From: anyNode, To: anyNode, Tier: 2, Concurrent: true})
	// The target must BIND somewhere (else the to-binds-nothing check fires first),
	// but NOT on the concurrent cone — so an isolated node reaches it. The concurrent
	// cone stays blind and never reaches it, exercising the blind-frontier caution.
	g.Nodes = append(g.Nodes, graph.Node{FQN: "example.com/blindsvc/internal/store.isolatedDeleter"})
	g.Edges = append(g.Edges, graph.Edge{From: "example.com/blindsvc/internal/store.isolatedDeleter", To: "boundary:db DELETE", Tier: 1, Boundary: "db"})
	rule := policy.ConcurrentRule{Name: "no-async-deletes", To: []string{"boundary:db DELETE"}}
	res := Check(concurrentPolicy(rule), graph.NewIndex(g))
	if c := res.Cautions(); len(c) != 1 || !strings.Contains(c[0].Summary, "frontier is blind") {
		t.Fatalf("want one blind-frontier caution, got %v", res.Findings)
	}

	rule.RequireProof = true
	res = Check(concurrentPolicy(rule), graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "require_proof") {
		t.Fatalf("require_proof must escalate, got %v", res.Findings)
	}
}

// TestConcurrentUnresolvedGoDispatchBlinds is the C-6 regression: a graph with a
// bindable boundary target, ZERO concurrent edges, and a ConcurrentDispatch blind
// spot (an unresolved `go f()` at a spawning function that never enters the
// concurrent cone) must not pass vacuously. The old blind probe surveyed only the
// resolved cone, so it saw nothing and returned silent green even under
// require_proof; the probe now surveys ConcurrentDispatch blind spots graph-wide.
func TestConcurrentUnresolvedGoDispatchBlinds(t *testing.T) {
	mk := func() *graph.Graph {
		return &graph.Graph{
			Algo: "rta",
			Nodes: []graph.Node{
				{FQN: "example.com/x/app.Handle"},
				{FQN: "example.com/x/store.deleteRows"},
			},
			Edges: []graph.Edge{
				// A bindable db DELETE reached only synchronously — no concurrent edge.
				{From: "example.com/x/store.deleteRows", To: "boundary:db DELETE rows", Tier: 1, Boundary: "db"},
			},
			BlindSpots: []graph.BlindSpot{
				// Unresolved `go someFuncValue()`: produces no edge, so nothing enters
				// the concurrent cone — only this blind spot at the spawning function.
				{Kind: "ConcurrentDispatch", Site: "example.com/x/app.Handle", Detail: "go f()"},
			},
		}
	}
	rule := policy.ConcurrentRule{Name: "no-async-deletes", To: []string{"boundary:db DELETE"}}

	res := Check(concurrentPolicy(rule), graph.NewIndex(mk()))
	if c := res.Cautions(); len(c) != 1 || !strings.Contains(c[0].Summary, "frontier is blind") {
		t.Fatalf("an unresolved go-dispatch must blind the concurrent probe (caution), got %v", res.Findings)
	}
	if len(res.Violations()) != 0 {
		t.Fatalf("without require_proof the blind dispatch is a caution, not a violation: %v", res.Findings)
	}

	rule.RequireProof = true
	res = Check(concurrentPolicy(rule), graph.NewIndex(mk()))
	if v := res.Violations(); len(v) != 1 || !strings.Contains(v[0].Summary, "require_proof") {
		t.Fatalf("require_proof must escalate the blind concurrent dispatch, got %v", res.Findings)
	}
}

// RF-2: findings are a set, not a multiset. The same function spawned from two
// goroutine sites — and its boundary effect reached both directly and through
// the cone — is one finding per (from, target) pair.
func TestConcurrentDuplicateSpawnsSingleFinding(t *testing.T) {
	g := loadGraph(t, "layeredsvc.graph.json")
	const sUpdate = "(*example.com/layeredsvc/internal/store.Store).UpdateUser"
	g.Edges = append(g.Edges,
		graph.Edge{From: "(*example.com/layeredsvc/internal/app.Service).UpdateProfile", To: sUpdate, Tier: 2, Concurrent: true},
		graph.Edge{From: "(*example.com/layeredsvc/internal/handler.Server).UpdateUser", To: sUpdate, Tier: 2, Concurrent: true},
	)
	rule := policy.ConcurrentRule{Name: "no-async-writes", To: []string{"boundary:db UPDATE"}}
	res := Check(concurrentPolicy(rule), graph.NewIndex(g))
	if v := res.Violations(); len(v) != 1 {
		t.Fatalf("two spawn sites of one function must yield one finding, got %v", v)
	}
}
