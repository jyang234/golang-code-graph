package fitness

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// A layering allow for the skip edge is LIVE on the graph that has the edge
// and DEAD once the edge is gone — the plan's "deleting the allow-listed edge
// flags the entry as dead on the next run".
func TestExceptionsLayeringLiveness(t *testing.T) {
	p := layeredPolicy()
	p.Layering.Allow = []policy.Exception{{
		From: hGetUserFast, To: sSelectUser, Reason: "reviewed fast path",
	}}

	withEdge := graph.NewIndex(loadGraph(t, "layeredsvc.branch-skip.graph.json"))
	xs := Exceptions(p, withEdge)
	if len(xs) != 1 || xs[0].Dead {
		t.Fatalf("allow should be LIVE while the skip edge exists: %+v", xs)
	}

	without := graph.NewIndex(loadGraph(t, "layeredsvc.graph.json"))
	xs = Exceptions(p, without)
	if len(xs) != 1 || !xs[0].Dead {
		t.Fatalf("allow should be DEAD once the skip edge is gone: %+v", xs)
	}
}

func TestExceptionsPassThroughLiveness(t *testing.T) {
	p := &policy.Policy{Service: "layeredsvc", Version: 1, MustPassThrough: []policy.PassRule{{
		Name:    "app-guards-db",
		From:    []string{policy.EntrypointSelector},
		To:      []string{"boundary:db"},
		Through: []string{"(*example.com/layeredsvc/internal/app.Service)"},
		Allow:   []policy.Exception{{From: hGetUserFast, Reason: "reviewed read-only bypass"}},
	}}}

	withBypass := graph.NewIndex(loadGraph(t, "layeredsvc.branch-skip.graph.json"))
	xs := Exceptions(p, withBypass)
	if len(xs) != 1 || xs[0].Dead {
		t.Fatalf("pass-through allow should be LIVE while the bypass exists: %+v", xs)
	}

	clean := graph.NewIndex(loadGraph(t, "layeredsvc.graph.json"))
	xs = Exceptions(p, clean)
	if len(xs) != 1 || !xs[0].Dead {
		t.Fatalf("pass-through allow should be DEAD on the clean graph: %+v", xs)
	}
}

// Two blind_spot_ratchet entries at the same Site differing only in Kind carry
// empty From/To, so without Kind in the sort key they tie on every key and the
// unstable sort flaps run-to-run. The output must be ordered totally and
// deterministically (Kind ascending here): "Alpha" before "Zeta".
func TestExceptionsSortIsTotalOrderOnKind(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{})
	p := &policy.Policy{Service: "svc", Version: 1, BlindSpotRatchet: &policy.BlindSpotRatchet{
		Allow: []policy.BlindSpotException{
			{Kind: "Zeta", Site: "pkg/x", Reason: "z"},
			{Kind: "Alpha", Site: "pkg/x", Reason: "a"},
		},
	}}
	for i := 0; i < 64; i++ {
		xs := Exceptions(p, ix)
		if len(xs) != 2 {
			t.Fatalf("want 2 entries, got %d: %+v", len(xs), xs)
		}
		if xs[0].Kind != "Alpha" || xs[1].Kind != "Zeta" {
			t.Fatalf("non-deterministic / non-total order on Kind: got [%q, %q]", xs[0].Kind, xs[1].Kind)
		}
	}
}

func TestExceptionsBlindSpotLiveness(t *testing.T) {
	blind := graph.NewIndex(loadGraph(t, "blindsvc.graph.json"))
	spot := blind.BlindSpots()[0]
	p := &policy.Policy{Service: "blindsvc", Version: 1, BlindSpotRatchet: &policy.BlindSpotRatchet{
		Allow: []policy.BlindSpotException{
			{Kind: spot.Kind, Site: spot.Site, Reason: "audited"},
			{Kind: "reflect", Site: "example.com/blindsvc/internal/gone.Decode", Reason: "stale"},
		},
	}}
	xs := Exceptions(p, blind)
	if len(xs) != 2 {
		t.Fatalf("want 2 audited entries, got %v", xs)
	}
	byDead := map[bool]int{}
	for _, x := range xs {
		byDead[x.Dead]++
	}
	if byDead[false] != 1 || byDead[true] != 1 {
		t.Fatalf("want one LIVE and one DEAD, got %+v", xs)
	}
	if DeadCount(xs) != 1 {
		t.Errorf("DeadCount = %d, want 1", DeadCount(xs))
	}
}

// RF-2: the case the count-equality differential got wrong. Removing this
// allow entry swaps the rule's blind-frontier Caution for a bypass Violation —
// the finding COUNT stays equal, but the entry is actively suppressing a
// gate-failing violation and must be LIVE. Set-based attribution cannot be
// fooled by the swap.
func TestExceptionsLiveOnCautionViolationSwap(t *testing.T) {
	p := &policy.Policy{Service: "blindsvc", Version: 1, MustPassThrough: []policy.PassRule{{
		Name:    "audit-guards-bus",
		From:    []string{policy.EntrypointSelector},
		To:      []string{"boundary:bus PUBLISH user.created"}, // exactly one bypass target
		Through: []string{"example.com/blindsvc/internal/audit.Check"},
		Allow:   []policy.Exception{{To: "boundary:bus PUBLISH user.created", Reason: "reviewed"}},
	}}}
	ix := graph.NewIndex(loadGraph(t, "blindsvc.graph.json")) // blind cone: caution when allowed

	// Sanity: the swap shape holds — allowed run yields one caution, stripped
	// run yields violations; counts may coincide, which is the trap.
	base := Check(p, ix)
	if len(base.Violations()) != 0 || len(base.Cautions()) != 1 {
		t.Fatalf("fixture drifted: want exactly one caution with the allow present, got %v", base.Findings)
	}

	xs := Exceptions(p, ix)
	if len(xs) != 1 || xs[0].Dead {
		t.Fatalf("entry suppressing the only bypass must be LIVE: %+v", xs)
	}
}
