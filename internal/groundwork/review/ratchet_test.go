package review

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// The blind-spot ratchet (GX-2): no new blind spots base→branch without an
// allow-list entry. Observe-only by default; merge-blocking when the policy
// sets blind_spot_ratchet.gate.

const reflectSite = "(*example.com/layeredsvc/internal/app.Service).GetProfile"

func withRatchet(t *testing.T, r *policy.BlindSpotRatchet) *policy.Policy {
	t.Helper()
	p := loadPolicy(t)
	p.BlindSpotRatchet = r
	return p
}

// addBlindSpot returns the base graph plus one new blind spot (and nothing else).
func branchWithBlindSpot(t *testing.T, kind, site, detail string) *graph.Graph {
	t.Helper()
	g := loadGraph(t, "layeredsvc.graph.json")
	g.BlindSpots = append(g.BlindSpots, graph.BlindSpot{Kind: kind, Site: site, Detail: detail})
	return g
}

func TestReviewReportsNewBlindSpot(t *testing.T) {
	p := loadPolicy(t) // no ratchet configured: reported, never gated
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")

	a := Review(p, base, branch)
	if len(a.NewBlindSpots) != 1 || a.NewBlindSpots[0].Site != reflectSite {
		t.Fatalf("new blind spots = %v, want the reflect site", a.NewBlindSpots)
	}
	if a.Verdict == Block {
		t.Fatalf("verdict = BLOCK without gate: true; ratchet must be observe-only by default")
	}
}

// A body-only change that introduces reflection must not read as "the graph has
// nothing to say" — the graph's knowledge of the code shrank, and that IS a
// signal.
func TestNewBlindSpotSuppressesAbstention(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")

	a := Review(p, base, branch)
	if a.Verdict == NoStructuralSignal {
		t.Fatal("verdict abstained despite a new blind spot")
	}
}

func TestGateBlocksOnNewBlindSpot(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{Gate: true})
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")

	if a := Review(p, base, branch); a.Verdict != Block {
		t.Fatalf("review verdict = %s, want BLOCK with gate: true", a.Verdict)
	}
	g := Gate(p, base, branch, nil)
	if g.Pass {
		t.Fatal("gate passed despite a gated new blind spot")
	}
	if len(g.NewBlindSpots) != 1 || g.NewBlindSpots[0].Site != reflectSite {
		t.Fatalf("gate blind spots = %v, want the reflect site", g.NewBlindSpots)
	}
}

func TestAllowSuppressesExactlyThatSite(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{
		Gate:  true,
		Allow: []policy.BlindSpotException{{Kind: "reflect", Site: reflectSite, Reason: "audited codec"}},
	})
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")
	branch.BlindSpots = append(branch.BlindSpots, graph.BlindSpot{Kind: "reflect", Site: hGetUser, Detail: "reflect.TypeOf call"})

	a := Review(p, base, branch)
	if len(a.NewBlindSpots) != 1 || a.NewBlindSpots[0].Site != hGetUser {
		t.Fatalf("new blind spots = %v, want only the unallowed site", a.NewBlindSpots)
	}
	if g := Gate(p, base, branch, nil); g.Pass {
		t.Fatal("gate passed despite an unallowed new blind spot")
	}
}

func TestAllowKindMismatchDoesNotSuppress(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{
		Allow: []policy.BlindSpotException{{Kind: "HighFanOut", Site: reflectSite, Reason: "interface-dense"}},
	})
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")

	if a := Review(p, base, branch); len(a.NewBlindSpots) != 1 {
		t.Fatalf("new blind spots = %v; an allow entry for another kind must not suppress", a.NewBlindSpots)
	}
}

func TestBaseEqualReportsNoBlindSpots(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{Gate: true})
	base := loadGraph(t, "blindsvc.graph.json") // fixture with pre-existing blind spots
	a := Review(p, base, base)
	if len(a.NewBlindSpots) != 0 {
		t.Fatalf("identical graphs reported new blind spots: %v", a.NewBlindSpots)
	}
	if a.Verdict != NoStructuralSignal {
		t.Fatalf("verdict = %s, want NO-STRUCTURAL-SIGNAL on identical graphs", a.Verdict)
	}
	if g := Gate(p, base, base, nil); !g.Pass {
		t.Fatal("gate blocked on pre-existing blind spots; the ratchet is base→branch drift only")
	}
}

func TestRemovedBlindSpotNotReported(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{Gate: true})
	base := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")
	branch := loadGraph(t, "layeredsvc.graph.json") // the blind spot is gone

	a := Review(p, base, branch)
	if len(a.NewBlindSpots) != 0 {
		t.Fatalf("removing a blind spot reported drift: %v", a.NewBlindSpots)
	}
	if a.Verdict == Block {
		t.Fatal("removing a blind spot blocked the gate")
	}
}

// Identity is (kind, site): a re-worded Detail between base and branch must not
// resurface the same blind spot as "new" (the D-OB6 key-stability discipline).
func TestDetailChangeIsNotDrift(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{Gate: true})
	base := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflection used for decoding")

	a := Review(p, base, branch)
	if len(a.NewBlindSpots) != 0 {
		t.Fatalf("detail prose change reported as drift: %v", a.NewBlindSpots)
	}
}

func TestRatchetDeterministic(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{Gate: true})
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")
	if a, b := Review(p, base, branch), Review(p, base, branch); a.Digest != b.Digest {
		t.Fatalf("non-deterministic digest: %s vs %s", a.Digest, b.Digest)
	}
}
