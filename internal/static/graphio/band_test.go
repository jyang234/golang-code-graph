package graphio

import "testing"

// TestClassifyBand pins the name rule branch by branch — the vocabulary and the
// MOST-SPECIFIC-FIRST ordering classifyBand promises. Each case names which signal it
// exercises so a future widening of a leaf set (or a re-order of the switch) fails
// loudly here rather than silently re-laning a component in a render.
func TestClassifyBand(t *testing.T) {
	cases := []struct {
		pkg  string
		want string
		why  string
	}{
		// tests is checked FIRST: a test-support package wins even when its name would
		// otherwise read as another lane.
		{"ex.com/svc/testutil", BandTests, "test* prefix on the leaf"},
		{"ex.com/svc/teststore", BandTests, "test* prefix beats the storeLeaf signal (order matters)"},

		// provisioning matches the WHOLE path, so a sub-package inherits the band.
		{"ex.com/svc/awsprovisioner", BandProvisioning, "aws in path"},
		{"ex.com/svc/awssns", BandProvisioning, "aws sub-package inherits provisioning"},
		{"ex.com/svc/provisioningoutbox", BandProvisioning, "provision in path"},
		{"ex.com/svc/sourceid", BandApplication, "no provisioning signal on its own (parent-path inheritance is a path fact, not a leaf one)"},
		{"ex.com/svc/reconciler", BandProvisioning, "reconcil in path"},

		// transport: leaf set OR the *handler suffix.
		{"ex.com/svc/api", BandTransport, "api leaf"},
		{"ex.com/svc/server", BandTransport, "server leaf"},
		{"ex.com/svc/delivery", BandTransport, "delivery leaf"},
		{"ex.com/svc/eventhandler", BandTransport, "*handler suffix"},

		// storage and infrastructure leaf sets.
		{"ex.com/svc/storage", BandStorage, "storage leaf"},
		{"ex.com/svc/store", BandStorage, "store leaf"},
		{"ex.com/svc/repository", BandStorage, "repository leaf"},
		{"ex.com/svc/bootstrap", BandInfrastructure, "bootstrap leaf"},
		{"ex.com/svc/config", BandInfrastructure, "config leaf"},

		// the disclosed fallback: a name with no role signal.
		{"ex.com/svc/app", BandApplication, "no role signal — disclosed fallback"},
		{"ex.com/svc/subscriptions", BandApplication, "a transport named for its domain falls to the fallback (the known 28/29 miss)"},
		{"ex.com/svc/transaction", BandApplication, "domain core — fallback"},
	}
	for _, c := range cases {
		if got := classifyBand(c.pkg); got != c.want {
			t.Errorf("classifyBand(%q) = %q, want %q (%s)", c.pkg, got, c.want, c.why)
		}
	}
}

// TestClassifyBandDeterministic guards the determinism the band field ships with
// (CLAUDE.md: a new name-derived axis is a pure function of its input, byte-stable
// run-to-run). classifyBand walks the leaf-set maps, so any arrival-order leak would
// surface as a run-to-run difference here.
func TestClassifyBandDeterministic(t *testing.T) {
	pkgs := []string{
		"ex.com/svc/api", "ex.com/svc/store", "ex.com/svc/awssns",
		"ex.com/svc/bootstrap", "ex.com/svc/testutil", "ex.com/svc/app",
	}
	first := make([]string, len(pkgs))
	for i, p := range pkgs {
		first[i] = classifyBand(p)
	}
	for run := 0; run < 50; run++ {
		for i, p := range pkgs {
			if got := classifyBand(p); got != first[i] {
				t.Fatalf("classifyBand(%q) not deterministic on run %d: %q vs %q", p, run, got, first[i])
			}
		}
	}
}

// TestRollupRootIsBandless pins the load-bearing invariant that the composition root is
// named by Role and NEVER carries a band — so a grouped render draws it outside the
// bands. classifyBand has no root case at all; this guards the populate hook honoring it.
func TestRollupRootIsBandless(t *testing.T) {
	r := compositionRootGraph().RollupByPackage()
	for _, c := range r.Components {
		if c.Role == RollupRoot && c.Band != "" {
			t.Errorf("the composition root must be bandless (named by Role), got Band=%q on %+v", c.Band, c)
		}
		if c.Role != RollupRoot && c.Band == "" {
			t.Errorf("a non-root component must carry a band, got none on %+v", c)
		}
	}
}
