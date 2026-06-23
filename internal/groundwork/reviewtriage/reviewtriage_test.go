package reviewtriage

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// TestBuildPartitionsVouchedAndFocus pins the core contract: a changed function with
// a fully-resolved effect surface is VOUCHED (and its complete evidence is rendered),
// while a changed function that touches a disclosed blind spot is FOCUS (with the
// reason rendered). Both functions changed (signature moved), so the partition — not
// the change detection — is what is under test.
func TestBuildPartitionsVouchedAndFocus(t *testing.T) {
	base := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "old"}, {FQN: "svc.Blind", Sig: "old"}},
	}
	branch := &graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.Clean", Sig: "new"}, {FQN: "svc.Blind", Sig: "new"}},
		Edges: []graph.Edge{
			{From: "svc.Clean", To: "boundary:db SELECT users", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{
			{Kind: "http", Name: "GET /clean", Fn: "svc.Clean"},
			{Kind: "http", Name: "GET /blind", Fn: "svc.Blind"},
		},
		BlindSpots: []graph.BlindSpot{{Kind: "reflect", Site: "svc.Blind", Detail: "reflective call"}},
	}

	rep := Build(base, branch)

	if got := len(rep.Vouched) + len(rep.Focus); got != 2 {
		t.Fatalf("want 2 changed functions partitioned, got %d (%+v / %+v)", got, rep.Vouched, rep.Focus)
	}
	if len(rep.Vouched) != 1 || rep.Vouched[0].FQN != "svc.Clean" {
		t.Errorf("svc.Clean should be VOUCHED (resolved effect, no blind spot): %+v", rep.Vouched)
	}
	if len(rep.Focus) != 1 || rep.Focus[0].FQN != "svc.Blind" {
		t.Errorf("svc.Blind should be FOCUS (reflection blind spot): %+v", rep.Focus)
	}
	if len(rep.Focus) == 1 && len(rep.Focus[0].Reasons) == 0 {
		t.Error("a FOCUS change must carry at least one reason the tool cannot vouch")
	}

	md := rep.RenderMarkdown()
	// The vouched change must expose its complete, checkable effect surface as evidence.
	if !strings.Contains(md, "db SELECT users") || !strings.Contains(md, "COMPLETE boundary-effect surface") {
		t.Errorf("vouched evidence (the resolved effect surface) not rendered:\n%s", md)
	}
	// The focus change must name the blind spot, and its evidence must read as a FLOOR.
	if !strings.Contains(md, "reflection") || !strings.Contains(md, "FLOOR") {
		t.Errorf("focus reason/floor not rendered:\n%s", md)
	}
}

// TestBuildNoStructuralChange: identical graphs ⇒ nothing to triage, and the render
// says so explicitly rather than emitting a blank page (silence is never a silent pass).
func TestBuildNoStructuralChange(t *testing.T) {
	g := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.A", Sig: "s"}}}
	rep := Build(g, g)
	if len(rep.Vouched)+len(rep.Focus) != 0 {
		t.Fatalf("identical graphs must yield no changed functions, got %+v / %+v", rep.Vouched, rep.Focus)
	}
	if !strings.Contains(rep.RenderMarkdown(), "No structural change detected") {
		t.Errorf("a no-change render must say so explicitly:\n%s", rep.RenderMarkdown())
	}
}
