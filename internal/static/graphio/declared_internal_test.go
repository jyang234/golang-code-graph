package graphio

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// TestMergeDeclaredBlindSpots covers the §8 enactment merge: a config-declared seam
// is added to the graph's blind spots with the ImpeachmentSeam kind by default, the
// reason as detail, deduped by (kind, site), and the result is deterministically
// sorted regardless of declaration order.
func TestMergeDeclaredBlindSpots(t *testing.T) {
	detected := []blindspots.BlindSpot{{Kind: blindspots.HighFanOut, Site: "ex.com/svc.fanout", Detail: "8 callees"}}
	cfg := &config.Config{}
	cfg.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
		{Site: "ex.com/svc.Seam", Reason: "ratified impeachment witness"},
		{Site: "ex.com/svc.Seam", Reason: "ratified impeachment witness"}, // exact dup ⇒ deduped
		{Site: ""}, // no site ⇒ skipped (nothing to blind)
	}

	got := mergeDeclaredBlindSpots(detected, cfg)
	if len(got) != 2 {
		t.Fatalf("want 2 blind spots (detected + one deduped declared), got %d: %+v", len(got), got)
	}

	var seam *blindspots.BlindSpot
	for i := range got {
		if got[i].Site == "ex.com/svc.Seam" {
			seam = &got[i]
		}
	}
	if seam == nil {
		t.Fatal("declared seam not merged")
	}
	if seam.Kind != blindspots.ImpeachmentSeam {
		t.Errorf("Kind = %q, want %q (default)", seam.Kind, blindspots.ImpeachmentSeam)
	}
	if seam.Detail != "ratified impeachment witness" {
		t.Errorf("Detail = %q, want the first declaration's reason (dedup keeps one)", seam.Detail)
	}

	// Determinism: shuffling the declaration order of distinct seams must not
	// change the output (the merge sorts the final set).
	a := &config.Config{}
	a.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
		{Site: "ex.com/svc.Alpha", Reason: "w1"}, {Site: "ex.com/svc.Beta", Reason: "w2"},
	}
	b := &config.Config{}
	b.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
		{Site: "ex.com/svc.Beta", Reason: "w2"}, {Site: "ex.com/svc.Alpha", Reason: "w1"},
	}
	ga, gb := mergeDeclaredBlindSpots(detected, a), mergeDeclaredBlindSpots(detected, b)
	if len(ga) != len(gb) {
		t.Fatalf("length differs: %d vs %d", len(ga), len(gb))
	}
	for i := range ga {
		if ga[i] != gb[i] {
			t.Errorf("merge is order-dependent at %d:\n %+v\n %+v", i, ga, gb)
		}
	}

	// No config ⇒ untouched.
	if out := mergeDeclaredBlindSpots(detected, &config.Config{}); len(out) != 1 {
		t.Errorf("empty config must not add blind spots, got %+v", out)
	}
}
