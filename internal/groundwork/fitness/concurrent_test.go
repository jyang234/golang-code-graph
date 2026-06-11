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
