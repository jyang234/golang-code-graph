package review

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// PC-2 route I/O deltas: per-route attribution of write-surface movement,
// which the global io_effects diff (set-based over boundary edges) cannot
// give. Disclosure only — no verdict change, no thresholds.

const (
	aUpdateProfile = "(*example.com/layeredsvc/internal/app.Service).UpdateProfile"
	hUpdateUser    = "(*example.com/layeredsvc/internal/handler.Server).UpdateUser"
)

// The defining test: a route loses its path to a shared write while another
// route keeps it. The global effect set is unchanged — io_effects is empty —
// and only the per-route section sees the lost write.
func TestLostWriteInvisibleGlobally(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	base.Edges = append(base.Edges, graph.Edge{From: hGetUser, To: aUpdateProfile, Tier: 2})
	branch := loadGraph(t, "layeredsvc.graph.json") // the GetUser→UpdateProfile path is severed

	a := Review(p, base, branch)
	if len(a.Effects) != 0 {
		t.Fatalf("io_effects = %v; the global effect set must be unchanged — that is the gap this section closes", a.Effects)
	}
	if len(a.RouteIO) != 1 || a.RouteIO[0].Route != hGetUser {
		t.Fatalf("route deltas = %+v, want exactly the GetUser row", a.RouteIO)
	}
	row := a.RouteIO[0]
	if row.Base.Writes != 2 || row.Branch.Writes != 0 || len(row.Removed) != 2 {
		t.Errorf("row = %+v, want 2→0 writes with both targets removed", row)
	}
	if a.Verdict != StructurallyClear {
		t.Errorf("verdict = %s; route deltas are disclosure, not a gate", a.Verdict)
	}
	if !strings.Contains(a.Render(), "no longer reached") {
		t.Error("rendered artifact hides the lost write")
	}
}

func TestNewWriteBehindExistingRoute(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.graph.json")
	// A bus publish, not a DB write: the fixture policy's must_not_reach rule
	// guards GetUser against DB mutations, and this test wants the route delta
	// and ratchet sections isolated from that rule.
	branch.Edges = append(branch.Edges, graph.Edge{
		From: aGetProfile, To: "boundary:bus PUBLISH user.updated", Tier: 1, Boundary: "outbound-async",
	})

	a := Review(p, base, branch)
	if len(a.RouteIO) != 1 || a.RouteIO[0].Route != hGetUser {
		t.Fatalf("route deltas = %+v, want the GetUser row", a.RouteIO)
	}
	if row := a.RouteIO[0]; row.Base.Writes != 0 || row.Branch.Writes != 1 ||
		len(row.Added) != 1 || row.Added[0] != "bus PUBLISH user.updated" {
		t.Errorf("row = %+v, want 0→1 with the publish added", row)
	}
	// The same change is also a new write target for the (ungated) ratchet.
	if len(a.NewWriteTargets) != 1 || a.NewWriteTargets[0] != "bus PUBLISH user.updated" {
		t.Errorf("new write targets = %v, want [bus PUBLISH user.updated]", a.NewWriteTargets)
	}
	if a.Verdict == Block {
		t.Error("ratchet must be observe-only without effect_ratchet.gate")
	}
}

// A renamed route HANDLER shows its counts on both one-sided rows, so a rename
// cannot launder a count change. The handler FQN moved (UpdateUser →
// UpdateUserV2) but the route name (PUT /users/{id}) is unchanged, so it is NOT
// a breaking entrypoint contract change: the contract is the route name, not the
// handler symbol (R10 — keying the entrypoint delta on the FQN over-fired on any
// handler-symbol move, including a plain rename, even with the route intact).
func TestRouteRenameShowsOneSidedRows(t *testing.T) {
	const renamed = "(*example.com/layeredsvc/internal/handler.Server).UpdateUserV2"
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.graph.json")
	for i := range branch.Nodes {
		if branch.Nodes[i].FQN == hUpdateUser {
			branch.Nodes[i].FQN = renamed
		}
	}
	for i := range branch.Edges {
		if branch.Edges[i].From == hUpdateUser {
			branch.Edges[i].From = renamed
		}
	}

	a := Review(p, base, branch)
	if len(a.RouteIO) != 2 {
		t.Fatalf("route deltas = %+v, want one-sided rows for both names", a.RouteIO)
	}
	var gone, added *RouteIODelta
	for i := range a.RouteIO {
		switch a.RouteIO[i].Route {
		case hUpdateUser:
			gone = &a.RouteIO[i]
		case renamed:
			added = &a.RouteIO[i]
		}
	}
	if gone == nil || gone.Branch != nil || gone.Base.Writes != 2 {
		t.Errorf("removed-route row = %+v, want base-only with 2 writes", gone)
	}
	if added == nil || added.Base != nil || added.Branch.Writes != 2 {
		t.Errorf("added-route row = %+v, want branch-only with 2 writes", added)
	}
	// The route name (PUT /users/{id}) is unchanged, so the handler rename is not
	// a breaking contract change — the structural delta still registers the move.
	if anyBreaking(a.Contract) {
		t.Errorf("a handler rename with a stable route name must not be a breaking contract change; got %+v", a.Contract)
	}
	if a.Verdict != StructurallyClear {
		t.Errorf("verdict = %s, want STRUCTURALLY-CLEAR for a handler rename with a stable route", a.Verdict)
	}
}

// A side counted over a blind frontier carries the marker: a delta against a
// blind side may be the graph's knowledge shifting, not the code's behavior.
func TestRouteDeltaBlindFrontierMarked(t *testing.T) {
	p := loadPolicy(t)
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.graph.json")
	branch.Edges = append(branch.Edges, graph.Edge{
		From: aGetProfile, To: "boundary:db INSERT log", Tier: 1, Boundary: "outbound-sync",
	})
	branch.BlindSpots = append(branch.BlindSpots, graph.BlindSpot{
		Kind: "HighFanOut", Site: aGetProfile, Detail: "interface dispatch",
	})

	a := Review(p, base, branch)
	if len(a.RouteIO) != 1 {
		t.Fatalf("route deltas = %+v, want the GetUser row", a.RouteIO)
	}
	row := a.RouteIO[0]
	if row.Base.Frontier != "resolved" || row.Branch.Frontier != "blind" {
		t.Errorf("frontiers = %s→%s, want resolved→blind", row.Base.Frontier, row.Branch.Frontier)
	}
	if !strings.Contains(a.Render(), "(frontier blind)") {
		t.Error("rendered artifact hides the blind frontier")
	}
}

func TestIdenticalGraphsNoRouteRows(t *testing.T) {
	p := loadPolicy(t)
	g := loadGraph(t, "layeredsvc.graph.json")
	a := Review(p, g, g)
	if len(a.RouteIO) != 0 || len(a.NewWriteTargets) != 0 {
		t.Fatalf("identical graphs produced route/effect drift: %+v %v", a.RouteIO, a.NewWriteTargets)
	}
	if a.Verdict != NoStructuralSignal {
		t.Errorf("verdict = %s, want NO-STRUCTURAL-SIGNAL", a.Verdict)
	}
}

// PC-1 in review: a refactor that renames the guarded symbol makes the rule's
// From bind nothing — the branch grows an inert-rule Caution the base did not
// have, and it surfaces as new. The new name must not share the old name as a
// prefix, or the exact-or-prefix matcher would still bind it.
func TestRenameToInertSurfacesNewCaution(t *testing.T) {
	const renamed = "(*example.com/layeredsvc/internal/handler.Server).FetchUser"
	p := loadPolicy(t) // must_not_reach names GetUser exactly
	base := loadGraph(t, "layeredsvc.graph.json")
	branch := loadGraph(t, "layeredsvc.graph.json")
	for i := range branch.Nodes {
		if branch.Nodes[i].FQN == hGetUser {
			branch.Nodes[i].FQN = renamed
		}
	}
	for i := range branch.Edges {
		if branch.Edges[i].From == hGetUser {
			branch.Edges[i].From = renamed
		}
	}

	a := Review(p, base, branch)
	found := false
	for _, c := range a.NewCautions {
		if c.Rule == "must_not_reach" && strings.Contains(c.Summary, "binds nothing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("new cautions = %v, want the inert-rule disclosure", a.NewCautions)
	}
}

// The born-inert escape (from the pressure test): a rule inert on BOTH graphs
// cautions identically on each, so the new-findings diff suppresses it forever.
// The absolute Liveness audit is what keeps it visible.
func TestBornInertRuleInvisibleInReviewButInAudit(t *testing.T) {
	p := loadPolicy(t)
	p.MustNotReach = append(p.MustNotReach, policy.ReachRule{
		Name: "ghost-rule", From: []string{"example.com/ghost"}, To: []string{"boundary:db"},
	})
	g := loadGraph(t, "layeredsvc.graph.json")

	a := Review(p, g, g)
	if len(a.NewCautions) != 0 {
		t.Fatalf("new cautions = %v; a born-inert rule must be suppressed by the diff (that is the documented escape)", a.NewCautions)
	}

	dead := false
	for _, l := range fitness.Liveness(p, graph.NewIndex(g)) {
		if l.Source == "must_not_reach:ghost-rule" && l.Field == "from" && l.Dead {
			dead = true
		}
	}
	if !dead {
		t.Fatal("the liveness audit must list the born-inert from as DEAD")
	}
}
