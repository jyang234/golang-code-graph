package review

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/impeach"
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

// TestImpeachmentSeamNotRatchetDrift pins the §13 enactment fence (#4): a
// CODEOWNER-gated config-declared ImpeachmentSeam blind spot rides into the branch
// graph, but the blind-spot ratchet must NOT treat it as undisclosed-dynamism
// drift. It is a reviewed ratification, not detected drift — arming the ratchet on
// it would make the impeachment enactment self-defeating (the ratified seam would
// re-block the very change that ratified it). Even with the ratchet gating, the
// seam alone must not appear in NewBlindSpots nor BLOCK.
func TestImpeachmentSeamNotRatchetDrift(t *testing.T) {
	p := withRatchet(t, &policy.BlindSpotRatchet{Gate: true})
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := branchWithBlindSpot(t, impeach.BlindSpotKindImpeachment, reflectSite, "ratified seam")

	a := Review(p, base, branch)
	for _, s := range a.NewBlindSpots {
		if s.Kind == impeach.BlindSpotKindImpeachment {
			t.Fatalf("config-declared ImpeachmentSeam armed the ratchet: %v", a.NewBlindSpots)
		}
	}
	// A reflect blind spot at the SAME site still arms it — the exclusion is scoped
	// to the seam kind, not the site, so real drift is not laundered through it.
	driftBranch := branchWithBlindSpot(t, "reflect", reflectSite, "reflect.ValueOf call")
	driftBranch.BlindSpots = append(driftBranch.BlindSpots, graph.BlindSpot{Kind: impeach.BlindSpotKindImpeachment, Site: reflectSite, Detail: "ratified seam"})
	if a := Review(p, base, driftBranch); a.Verdict != Block {
		t.Fatalf("verdict = %s, want BLOCK: a real reflect drift must still arm the ratchet alongside a seam", a.Verdict)
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

// must_pass_through reports every bypass pair (unlike must_not_reach's single
// witness) precisely so this works: a second bypass added on a branch must
// surface as a NEW violation even when the base already carries one.
func TestPassThroughSecondBypassIsNewViolation(t *testing.T) {
	p := loadPolicy(t)
	p.MustPassThrough = []policy.PassRule{{
		Name:    "app-guards-db",
		From:    []string{policy.EntrypointSelector},
		To:      []string{"boundary:db"},
		Through: []string{"(*example.com/layeredsvc/internal/app.Service)"},
	}}
	base := loadGraph(t, "layeredsvc.branch-skip.graph.json") // pre-existing bypass
	branch := loadGraph(t, "layeredsvc.branch-skip.graph.json")
	const v2Export = "(*example.com/layeredsvc/internal/handlerv2.Server).Export"
	branch.Nodes = append(branch.Nodes, graph.Node{FQN: v2Export, Sig: "func()", Tier: 1})
	branch.Edges = append(branch.Edges, graph.Edge{From: v2Export, To: sSelectUser, Tier: 2})

	a := Review(p, base, branch)
	var passViolations []Violation
	for _, v := range a.NewViolations {
		if v.Rule == "must_pass_through" {
			passViolations = append(passViolations, v)
		}
	}
	if len(passViolations) != 1 || passViolations[0].From != v2Export {
		t.Fatalf("want only the second bypass as new, got %v", a.NewViolations)
	}
}

// The obligation drift ratchet (O4/O5): a SATISFIED→VIOLATED flip surfaces as
// a NEW violation against the old base, and a detail-prose change does not.
func TestObligationFlipIsNewViolation(t *testing.T) {
	p := &policy.Policy{Service: "obligsvc", Version: 1}
	base := loadGraph(t, "obligsvc.graph.json")
	branch := loadGraph(t, "obligsvc.graph.json")
	for i := range branch.Obligations {
		if strings.HasSuffix(branch.Obligations[i].Fn, ".TransferDefer") {
			branch.Obligations[i].Status = "VIOLATED"
			branch.Obligations[i].Detail = "exit reachable without release"
		}
	}
	a := Review(p, base, branch)
	if a.Verdict != Block {
		t.Fatalf("verdict = %s, want BLOCK on a flipped obligation", a.Verdict)
	}
	if len(a.NewViolations) != 1 || !strings.Contains(a.NewViolations[0].From, "TransferDefer") {
		t.Fatalf("new violations = %v, want only the flipped TransferDefer", a.NewViolations)
	}
}

func TestObligationDetailChangeIsNotNew(t *testing.T) {
	p := &policy.Policy{Service: "obligsvc", Version: 1}
	base := loadGraph(t, "obligsvc.graph.json")
	branch := loadGraph(t, "obligsvc.graph.json")
	for i := range branch.Obligations {
		if branch.Obligations[i].Status == "VIOLATED" {
			branch.Obligations[i].Detail = "re-worded evidence prose"
		}
	}
	a := Review(p, base, branch)
	if len(a.NewViolations) != 0 {
		t.Fatalf("detail prose change reported as new: %v", a.NewViolations)
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

// RF-3: abstention follows the artifact, not the delta. A body-only change
// that surfaces a new obligation caution must not render as "the graph has
// nothing to say" — that early return would hide the exact disclosure the
// obligations feature exists to surface.
func TestNewCautionSuppressesAbstention(t *testing.T) {
	p := &policy.Policy{Service: "obligsvc", Version: 1}
	base := loadGraph(t, "obligsvc.graph.json")
	branch := loadGraph(t, "obligsvc.graph.json")
	branch.Obligations = append(branch.Obligations, graph.Obligation{
		Rule: "tx-must-close", Kind: "must-release",
		Fn: "example.com/obligsvc/internal/app.TransferDefer", Site: "internal/app/app.go:31",
		Status: "CANT-PROVE", Detail: "a body refactor made the release unprovable",
	})

	a := Review(p, base, branch)
	if a.Verdict == NoStructuralSignal {
		t.Fatal("verdict abstained despite a new caution")
	}
	if len(a.NewCautions) != 1 {
		t.Fatalf("new cautions = %v, want the CANT-PROVE disclosure", a.NewCautions)
	}
	if !strings.Contains(a.Render(), "cannot prove") {
		t.Error("rendered artifact hides the new caution")
	}
}

// Truly identical inputs still abstain: identical graphs produce identical
// findings, so no new anything, and NO-STRUCTURAL-SIGNAL stands.
func TestIdenticalGraphsStillAbstain(t *testing.T) {
	p := &policy.Policy{Service: "obligsvc", Version: 1}
	g := loadGraph(t, "obligsvc.graph.json") // carries violations AND cautions
	if a := Review(p, g, g); a.Verdict != NoStructuralSignal {
		t.Fatalf("identical graphs must abstain; got %s", a.Verdict)
	}
}
