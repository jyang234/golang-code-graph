package review

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// M-29: a pure emitter MOVE — the same boundary effect label emitted from a
// different function (a rename, extract-function, or consolidation) — must produce
// NO io_effects row. The per-edge delta reported it as a removed+added pair (a
// phantom "- label" and "+ label") because it keyed on the emitting edge; the
// set-based keying reports a change only when the target actually enters or leaves
// the branch.
func TestIOEffectsBlindToPureEmitterMove(t *testing.T) {
	p := loadPolicy(t)

	const (
		emitterA = "(*example.com/layeredsvc/internal/app.Service).GetProfile"
		emitterB = "(*example.com/layeredsvc/internal/app.Service).UpdateProfile"
	)
	label := graph.Edge{To: "boundary:db INSERT audit_log", Boundary: "outbound-sync"}

	base := loadGraph(t, "layeredsvc.graph.json")
	base.Edges = append(base.Edges, graph.Edge{From: emitterA, To: label.To, Tier: 1, Boundary: label.Boundary})

	branch := loadGraph(t, "layeredsvc.graph.json")
	// Same label, DIFFERENT emitter — a pure move; the effect SET is unchanged.
	branch.Edges = append(branch.Edges, graph.Edge{From: emitterB, To: label.To, Tier: 1, Boundary: label.Boundary})

	a := Review(p, base, branch)
	for _, e := range a.Effects {
		if e.Effect == "db INSERT audit_log" {
			t.Errorf("pure emitter move produced a phantom io_effects row: %+v (all: %+v)", e, a.Effects)
		}
	}
}

// Control: a genuinely NEW effect target (present in the branch, absent from the
// base under any emitter) is still reported once.
func TestIOEffectsReportsGenuineNewTarget(t *testing.T) {
	p := loadPolicy(t)
	const emitter = "(*example.com/layeredsvc/internal/app.Service).GetProfile"

	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.graph.json")
	branch.Edges = append(branch.Edges, graph.Edge{From: emitter, To: "boundary:db INSERT brand_new", Tier: 1, Boundary: "outbound-sync"})

	a := Review(p, base, branch)
	var count int
	for _, e := range a.Effects {
		if e.Effect == "db INSERT brand_new" {
			count++
			if e.Op != "+" || !e.Write {
				t.Errorf("new target row = %+v, want a single + write", e)
			}
		}
	}
	if count != 1 {
		t.Errorf("genuine new target should appear exactly once, got %d (all: %+v)", count, a.Effects)
	}
}
