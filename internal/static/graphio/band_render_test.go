package graphio

import (
	"reflect"
	"strings"
	"testing"
)

// bandedSampleGraph spans four bands plus the composition root, so a banded render must
// produce four lanes (transport / application / storage / infrastructure) with the root
// drawn outside them: a cmd root that constructs an api transport, which calls the app
// core, which writes a store and reads config.
func bandedSampleGraph() *Graph {
	return &Graph{
		CompositionRoots: []string{"ex.com/svc/cmd/svc"},
		Nodes: []Node{
			{FQN: "ex.com/svc/cmd/svc.main", Package: "ex.com/svc/cmd/svc"},
			{FQN: "(*ex.com/svc/api.H).Serve", Package: "ex.com/svc/api"},
			{FQN: "ex.com/svc/app.Run", Package: "ex.com/svc/app"},
			{FQN: "(*ex.com/svc/store.S).Save", Package: "ex.com/svc/store"},
			{FQN: "ex.com/svc/config.Load", Package: "ex.com/svc/config"},
		},
		Edges: []Edge{
			{From: "ex.com/svc/cmd/svc.main", To: "(*ex.com/svc/api.H).Serve"},
			{From: "(*ex.com/svc/api.H).Serve", To: "ex.com/svc/app.Run"},
			{From: "ex.com/svc/app.Run", To: "(*ex.com/svc/store.S).Save"},
			{From: "ex.com/svc/app.Run", To: "ex.com/svc/config.Load"},
		},
	}
}

// TestBandGroups pins the grouping helper directly: the known bands come out in
// bandRenderOrder, an unknown band (a declared override could introduce one) sorts after
// them, within-band order follows the input (the sorted node-emit order), and a bandless
// package lands in the remainder. This is the determinism contract the render rests on.
func TestBandGroups(t *testing.T) {
	bandOf := map[string]string{
		"a": BandStorage, "b": BandTransport, "c": "",
		"d": BandTransport, "e": "zeta-custom",
	}
	pkgs := []string{"a", "b", "c", "d", "e"}
	ordered, members, bandless := bandGroups(pkgs, bandOf)

	if want := []string{BandTransport, BandStorage, "zeta-custom"}; !reflect.DeepEqual(ordered, want) {
		t.Errorf("ordered bands = %v, want %v (canonical order, unknown last)", ordered, want)
	}
	if want := []string{"b", "d"}; !reflect.DeepEqual(members[BandTransport], want) {
		t.Errorf("transport members = %v, want %v (input order preserved)", members[BandTransport], want)
	}
	if want := []string{"c"}; !reflect.DeepEqual(bandless, want) {
		t.Errorf("bandless = %v, want %v", bandless, want)
	}
}

// TestRollupBandedMermaidValid pins that the banded render — which introduces subgraph
// lanes — is structurally valid Mermaid in both the plain and diff views.
func TestRollupBandedMermaidValid(t *testing.T) {
	g := bandedSampleGraph()
	if err := validateMermaid(g.RollupByPackage().Mermaid(RollupMermaidOptions{Bands: true})); err != nil {
		t.Errorf("banded rollup Mermaid invalid: %v", err)
	}
	branch := bandedSampleGraph()
	branch.Edges = branch.Edges[:2] // drop two edges so the diff has a delta to color
	if err := validateMermaid(RollupMermaidDiff(g, branch, RollupMermaidOptions{Bands: true})); err != nil {
		t.Errorf("banded rollup diff Mermaid invalid: %v", err)
	}
}

// TestRollupBandedLanesAndRootOutside pins the load-bearing layout: each band present
// becomes a lane, in canonical order, with the component boxes nested inside it — and the
// composition root is drawn OUTSIDE every lane (it is named by Role, not banded). Parsed
// structurally (subgraph depth) so the assertion is mechanical, not eyeballed.
func TestRollupBandedLanesAndRootOutside(t *testing.T) {
	out := bandedSampleGraph().RollupByPackage().Mermaid(RollupMermaidOptions{Bands: true})

	var laneOrder []string
	depth := 0
	rootDepth := -1
	for _, ln := range strings.Split(out, "\n") {
		s := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(s, "subgraph "):
			laneOrder = append(laneOrder, labelOf(s))
			depth++
		case s == "end":
			depth--
		case strings.Contains(s, ":::root"):
			rootDepth = depth // the composition-root box — must be at top level (0)
		}
	}

	wantLanes := []string{BandTransport, BandApplication, BandStorage, BandInfrastructure}
	if !reflect.DeepEqual(laneOrder, wantLanes) {
		t.Errorf("lane order = %v, want %v (canonical, one per present band)", laneOrder, wantLanes)
	}
	if rootDepth != 0 {
		t.Errorf("the composition root must be drawn OUTSIDE the bands (depth 0), got depth %d", rootDepth)
	}
}

// TestRollupBandsOffByDefault pins that banding is strictly opt-in: the zero-value option
// renders the flat layout (no subgraph lanes), so the default output is byte-identical to
// the pre-band render and a host that dislikes subgraphs is unaffected.
func TestRollupBandsOffByDefault(t *testing.T) {
	r := bandedSampleGraph().RollupByPackage()
	flat := r.Mermaid(RollupMermaidOptions{})
	if strings.Contains(flat, "subgraph ") {
		t.Errorf("default (bands off) render must not emit subgraph lanes:\n%s", flat)
	}
	if banded := r.Mermaid(RollupMermaidOptions{Bands: true}); !strings.Contains(banded, "subgraph ") {
		t.Errorf("bands-on render must emit subgraph lanes:\n%s", banded)
	}
}

// TestRollupBandedDeterministic is the determinism guard the banded ordering path ships
// with (CLAUDE.md: a new ordering path ships with a determinism test). Banding walks the
// per-band member maps, so any arrival-order leak would surface here — in both the plain
// and the diff render, each its own id-allocation path.
func TestRollupBandedDeterministic(t *testing.T) {
	g := bandedSampleGraph()
	base := bandedSampleGraph()
	base.Edges = base.Edges[:2]
	opts := RollupMermaidOptions{Bands: true}
	first := g.RollupByPackage().Mermaid(opts)
	firstDiff := RollupMermaidDiff(base, g, opts)
	for i := 0; i < 50; i++ {
		if m := g.RollupByPackage().Mermaid(opts); m != first {
			t.Fatalf("banded rollup Mermaid not deterministic on run %d:\n%s\nvs\n%s", i, m, first)
		}
		if m := RollupMermaidDiff(base, g, opts); m != firstDiff {
			t.Fatalf("banded rollup diff Mermaid not deterministic on run %d:\n%s\nvs\n%s", i, m, firstDiff)
		}
	}
}

// labelOf returns the quoted label of a Mermaid line ("subgraph band_x[\"transport\"]"
// → "transport"), for structural assertions on the rendered output.
func labelOf(line string) string {
	lo := strings.IndexByte(line, '"')
	hi := strings.LastIndexByte(line, '"')
	if lo >= 0 && hi > lo {
		return line[lo+1 : hi]
	}
	return ""
}
