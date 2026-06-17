package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// fitness surfaces the ratchet-coupling caution (effect ratchet gating, blind-spot
// ratchet not) as a graph-independent Caution that NEVER fails the gate — the
// disclosure of a config soundness gap, not a broken invariant. It is silent when
// both ratchets gate. The wording comes from the shared policy helper, so this test
// confirms fitness wires it through and keeps it advisory; the truth table itself is
// pinned in policy's TestEffectRatchetCouplingCaution.
func TestCheckSurfacesRatchetCouplingAsNonGatingCaution(t *testing.T) {
	g := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.A", Tier: 1}}}
	ix := graph.NewIndex(g)

	// Escape config: effect ratchet gates, blind-spot ratchet observe-only.
	escape := &policy.Policy{
		Service: "svc", Version: 1,
		EffectRatchet:    &policy.EffectRatchet{Gate: true},
		BlindSpotRatchet: &policy.BlindSpotRatchet{Gate: false},
	}
	res := Check(escape, ix)
	if !res.OK() {
		t.Fatal("the coupling caution must never fail the gate — it is a disclosure, not a violation")
	}
	var found *Finding
	for i, f := range res.Findings {
		if f.Rule == "effect_ratchet" && strings.Contains(f.Summary, "blind_spot_ratchet") {
			found = &res.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("escape config must surface the coupling caution; findings=%+v", res.Findings)
	}
	if found.Severity != Caution {
		t.Errorf("coupling finding must be a Caution, got %v", found.Severity)
	}

	// Both gating: no coupling caution.
	both := &policy.Policy{
		Service: "svc", Version: 1,
		EffectRatchet:    &policy.EffectRatchet{Gate: true},
		BlindSpotRatchet: &policy.BlindSpotRatchet{Gate: true},
	}
	for _, f := range Check(both, ix).Findings {
		if f.Rule == "effect_ratchet" && strings.Contains(f.Summary, "blind_spot_ratchet") {
			t.Errorf("both ratchets gating must not surface the coupling caution; got %+v", f)
		}
	}
}
