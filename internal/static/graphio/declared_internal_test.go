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
		{Site: "", Reason: "x"}, // no site ⇒ skipped (nothing to blind)
	}

	got, err := mergeDeclaredBlindSpots(detected, cfg)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
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
		t.Errorf("Detail = %q, want the declared reason", seam.Detail)
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
	ga, errA := mergeDeclaredBlindSpots(detected, a)
	gb, errB := mergeDeclaredBlindSpots(detected, b)
	if errA != nil || errB != nil {
		t.Fatalf("merge errors: %v / %v", errA, errB)
	}
	if len(ga) != len(gb) {
		t.Fatalf("length differs: %d vs %d", len(ga), len(gb))
	}
	for i := range ga {
		if ga[i] != gb[i] {
			t.Errorf("merge is order-dependent at %d:\n %+v\n %+v", i, ga, gb)
		}
	}

	// No config ⇒ untouched.
	out, err := mergeDeclaredBlindSpots(detected, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("empty config must not add blind spots, got %+v", out)
	}
}

// TestMergeDeclaredBlindSpotsDedupTieBreakIsIntrinsic pins the determinism fix
// (#20): when two entries collide on (kind, site) but carry different Detail, the
// kept Detail is the lexically-smallest — an INTRINSIC tie-break, not arrival
// order. Presenting the same colliding pair in either order must yield the same
// merged manifest, so the gated blind-spots are byte-identical run-to-run.
func TestMergeDeclaredBlindSpotsDedupTieBreakIsIntrinsic(t *testing.T) {
	mk := func(d1, d2 string) []blindspots.BlindSpot {
		cfg := &config.Config{}
		cfg.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
			{Site: "ex.com/svc.Seam", Reason: d1},
			{Site: "ex.com/svc.Seam", Reason: d2},
		}
		got, err := mergeDeclaredBlindSpots(nil, cfg)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		return got
	}
	fwd := mk("aaa", "zzz")
	rev := mk("zzz", "aaa")
	if len(fwd) != 1 || len(rev) != 1 {
		t.Fatalf("collision must collapse to one: %+v / %+v", fwd, rev)
	}
	if fwd[0] != rev[0] {
		t.Fatalf("dedup tie-break is arrival-dependent: %+v vs %+v", fwd[0], rev[0])
	}
	if fwd[0].Detail != "aaa" {
		t.Errorf("kept Detail = %q, want the lexically-smallest %q", fwd[0].Detail, "aaa")
	}
}

// TestMergeDeclaredBlindSpotsPreservesDistinctDetected guards against collapsing two
// DISTINCT auto-detected blind spots that share (kind, site) but differ in Detail —
// e.g. two over-threshold dispatch sites in one function, each a separate HighFanOut
// disclosure with its own callee count. The presence of an unrelated config-declared
// seam must NOT drop one of them (the fail-OPEN regression a review caught): detected
// spots pass through verbatim, only declared seams dedup among themselves.
func TestMergeDeclaredBlindSpotsPreservesDistinctDetected(t *testing.T) {
	detected := []blindspots.BlindSpot{
		{Kind: blindspots.HighFanOut, Site: "ex.com/svc.Handle", Detail: "resolves to 5 candidate callees"},
		{Kind: blindspots.HighFanOut, Site: "ex.com/svc.Handle", Detail: "resolves to 7 candidate callees"},
	}
	cfg := &config.Config{}
	cfg.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
		{Site: "ex.com/svc.Other", Reason: "ratified seam elsewhere"},
	}
	got, err := mergeDeclaredBlindSpots(detected, cfg)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// Both detected HighFanOut rows must survive (2) plus the unrelated declared seam (1).
	highFanOut := 0
	for _, b := range got {
		if b.Kind == blindspots.HighFanOut && b.Site == "ex.com/svc.Handle" {
			highFanOut++
		}
	}
	if highFanOut != 2 {
		t.Fatalf("distinct detected blind spots collapsed: %d HighFanOut rows survived, want 2: %+v", highFanOut, got)
	}
	if len(got) != 3 {
		t.Errorf("want 2 detected + 1 declared = 3, got %d: %+v", len(got), got)
	}
}

// TestMergeDeclaredBlindSpotsRedundantSeamYieldsToDetected pins that a declared seam
// at a (kind, site) ALREADY auto-detected is dropped as redundant — the authoritative
// detected disclosure text wins, and the declaration adds no second row.
func TestMergeDeclaredBlindSpotsRedundantSeamYieldsToDetected(t *testing.T) {
	detected := []blindspots.BlindSpot{
		{Kind: blindspots.Reflect, Site: "ex.com/svc.Decode", Detail: "reflective call; downstream edges are invisible to the static call graph"},
	}
	cfg := &config.Config{}
	cfg.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
		{Site: "ex.com/svc.Decode", Kind: "reflect", Reason: "AAA ratified"}, // sorts lexically smaller, must NOT win
	}
	got, err := mergeDeclaredBlindSpots(detected, cfg)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("redundant declared seam added a row: %+v", got)
	}
	if got[0].Detail != "reflective call; downstream edges are invisible to the static call graph" {
		t.Errorf("declared reason overrode the authoritative detected Detail: %q", got[0].Detail)
	}
}

// TestMergeDeclaredBlindSpotsRejectsUnknownKind pins the validation (#7): a seam
// declared with a Kind outside the recognized set is a config error, never a silent
// passthrough of an unknown category onto the gated artifact.
func TestMergeDeclaredBlindSpotsRejectsUnknownKind(t *testing.T) {
	cfg := &config.Config{}
	cfg.Static.DeclaredBlindSpots = []config.DeclaredBlindSpot{
		{Site: "ex.com/svc.Seam", Kind: "NotARealKind", Reason: "typo"},
	}
	if _, err := mergeDeclaredBlindSpots(nil, cfg); err == nil {
		t.Fatal("an unrecognized blind-spot kind must be rejected, got nil error")
	}
}
