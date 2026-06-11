package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
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
