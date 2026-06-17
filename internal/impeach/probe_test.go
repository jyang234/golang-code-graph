package impeach

import (
	"os"
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

// The committed behavioral corpus in this repo is loansvc-only (the other
// services ship a static graph golden but no captured traces), so real-corpus
// breadth is over loansvc FLOWS — the http happy path, its richer variant that
// also publishes the dynamic-bus `loan.review`, and the consumer entry. The
// cross-graph over-suppression controls below add SERVICE breadth synthetically.
const (
	loansvcTraceRich     = "../../testdata/fixtures/loansvc/flows/testdata/flows/post_loan_application.golden.json"
	loansvcTraceConsumer = "../../testdata/fixtures/loansvc/flows/testdata/flows/consume_payment_settled.golden.json"
	obligsvcGraph        = "../../testdata/groundwork/goldens/obligsvc.graph.json"
	blindsvcGraph        = "../../testdata/groundwork/goldens/blindsvc.graph.json"
)

// loadTraceOptional loads a committed trace that lives under another module's
// testdata; a missing file is skipped (not failed), since this package does not
// own it.
func loadTraceOptional(t *testing.T, path string) *ir.CanonicalTrace {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("optional corpus %s unavailable: %v", path, err)
	}
	tr, err := ir.Load(b)
	if err != nil {
		t.Fatalf("load trace %s: %v", path, err)
	}
	return tr
}

// TestProbeRealCorpora widens the Phase-0 go/no-go probe (§10) across every
// committed loansvc flow: each must yield ZERO candidates on the sound graph —
// the analyzer is not impeached by any captured behavior — while disclosing the
// reachable-but-unexercised surface as coverage gaps. The richer flow exercises
// the dynamic-bus RECLAIMED-LIVE exclusion on real data (it publishes
// `loan.review`, which the graph names only as `PUBLISH <dynamic>`).
func TestProbeRealCorpora(t *testing.T) {
	ix := loadIndex(t, loansvcGraph)

	cases := []struct {
		name  string
		trace *ir.CanonicalTrace
	}{
		{"http-happy", loadTrace(t, loansvcTrace)},
		{"http-rich", loadTraceOptional(t, loansvcTraceRich)},
		{"consumer", loadTraceOptional(t, loansvcTraceConsumer)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Audit("loansvc", ix, []*ir.CanonicalTrace{c.trace})
			// Determinism across runs.
			if r.Digest != Audit("loansvc", ix, []*ir.CanonicalTrace{c.trace}).Digest {
				t.Error("non-deterministic report")
			}
			if len(r.Candidates) != 0 {
				t.Errorf("%s: %d candidate(s) on a sound graph, want 0: %+v", c.name, len(r.Candidates), r.Candidates)
			}
			t.Logf("loansvc/%s: 0 candidates, %d coverage gaps %v", c.name, len(r.CoverageGaps), r.CoverageGaps)
		})
	}
}

// TestProbeNoOverSuppression is the SERVICE-breadth measurement the plan asks
// for: across graphs with DIFFERENT disclosed-blind-spot densities — loansvc
// (severed closures + dynamic bus), obligsvc (none), blindsvc (reflect + unsafe
// + dynamic bus) — an observed DB write the graph models no emitter for, and no
// DB-family blind spot covers, must STILL surface as an ABSENT candidate. This
// proves the frontier/blind-spot seeding is scoped, not a blanket amnesty: a
// bus/compute blind disclosure does not swallow an unmodeled database effect, so
// the cell is not a dead detector on a blind-heavy graph.
func TestProbeNoOverSuppression(t *testing.T) {
	unmodeled := func() *ir.CanonicalTrace {
		return &ir.CanonicalTrace{Flow: "probe", Service: "svc", Root: &ir.CanonicalSpan{
			Op: "ENTRY", Kind: ir.KindServer,
			Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
				{Op: "DB postgres DELETE shadow_ledger", Kind: ir.KindClient},
			}}},
		}}
	}
	for _, g := range []struct{ name, path string }{
		{"loansvc", loansvcGraph},
		{"obligsvc", obligsvcGraph},
		{"blindsvc", blindsvcGraph},
	} {
		t.Run(g.name, func(t *testing.T) {
			ix := loadIndex(t, g.path)
			r := Audit(g.name, ix, []*ir.CanonicalTrace{unmodeled()})
			if len(r.Candidates) != 1 || r.Candidates[0].Effect != "db DELETE shadow_ledger" || r.Candidates[0].Claim.Reachability != ReachAbsent {
				t.Fatalf("%s: cell over-suppressed; want 1 ABSENT db DELETE shadow_ledger, got %+v", g.name, r.Candidates)
			}
			t.Logf("%s: cell fires on unmodeled db write (no over-suppression)", g.name)
		})
	}
}
