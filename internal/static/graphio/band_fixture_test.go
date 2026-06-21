package graphio_test

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

// TestBandFixtureScoring is this repo's equivalent of the field report's fleet scoring
// (Appendix A): the tool deriving EVERY component's band itself, on the committed
// eventbussvc fixture, through the real RollupByPackage — not a hand-built graph. It
// pins the full component→band map so a regression that re-files a different component
// (or re-orders the classifier switch) fails loudly with the exact mislaning.
//
// The composition root carries Role and NO band; every first-party domain package
// carries a band and no Role. The fixture is Go-conventional, so the bands land exactly:
// the api/server transport edge, the store persistence lane, and the domain core (bus)
// at the disclosed application fallback.
func TestBandFixtureScoring(t *testing.T) {
	res, err := analyze.Analyze(eventbussvcDir())
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	r := g.RollupByPackage()

	const root = "example.com/eventbussvc"
	// wantBand is the band the tool must derive for each first-party component; the root
	// is intentionally absent (it is named by Role, not banded — asserted separately).
	wantBand := map[string]string{
		root + "/api":    graphio.BandTransport,
		root + "/server": graphio.BandTransport,
		root + "/store":  graphio.BandStorage,
		root + "/bus":    graphio.BandApplication, // domain core, no name signal — the disclosed fallback
	}

	seen := map[string]bool{}
	for _, c := range r.Components {
		if c.Package == root {
			if c.Role != graphio.RollupRoot {
				t.Errorf("the composition root must carry Role=%q, got %q", graphio.RollupRoot, c.Role)
			}
			if c.Band != "" {
				t.Errorf("the composition root must be bandless, got Band=%q", c.Band)
			}
			continue
		}
		want, ok := wantBand[c.Package]
		if !ok {
			t.Errorf("unexpected component %q (band %q) not in the pinned scoring map", c.Package, c.Band)
			continue
		}
		if c.Band != want {
			t.Errorf("component %q band = %q, want %q", c.Package, c.Band, want)
		}
		if c.Role != "" {
			t.Errorf("a non-root component %q must not carry a Role, got %q", c.Package, c.Role)
		}
		seen[c.Package] = true
	}
	for pkg := range wantBand {
		if !seen[pkg] {
			t.Errorf("expected component %q in the rollup, but it was absent", pkg)
		}
	}
}
