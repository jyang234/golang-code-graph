package fitness

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/obligations"
)

// TestObligationVerdictParity pins the obligation-verdict vocabulary the consumer
// (this package, checkObligations) matches against to the producer's constants
// (internal/static/obligations.Status). The two are DELIBERATELY decoupled — the
// graph JSON is the interface, so groundwork decodes the strings independently
// rather than importing the producer — and checkObligations fails closed on an
// unknown status. But per CLAUDE.md the parity of a value applied in two places
// must be pinned by a DIRECT named test (the schemadrift TestVerbParity pattern),
// not left to an indirect fixture golden: a producer that renamed "CANT-PROVE" to
// "CANT_PROVE" would silently turn every consumer match into a fall-through, and a
// golden might not exercise that verdict. This test is the tripwire — it imports
// the producer constant ONLY here, in a test, keeping the production decoupling.
func TestObligationVerdictParity(t *testing.T) {
	cases := []struct {
		consumer string
		producer obligations.Status
	}{
		{obligationViolated, obligations.Violated},
		{obligationCantProve, obligations.CantProve},
		{obligationUnmatched, obligations.Unmatched},
		{obligationSatisfied, obligations.Satisfied},
	}
	for _, c := range cases {
		if c.consumer != string(c.producer) {
			t.Errorf("obligation verdict drift: consumer %q != producer %q — the graph-JSON status vocabulary diverged; checkObligations would silently stop matching it", c.consumer, string(c.producer))
		}
	}

	// Completeness: every producer status except SATISFIED-with-no-finding must be
	// represented on the consumer side. Adding a new producer Status without teaching
	// the consumer must fail here rather than fall through to a silent no-op verdict.
	consumerKnows := map[string]bool{
		obligationViolated:  true,
		obligationCantProve: true,
		obligationUnmatched: true,
		obligationSatisfied: true,
	}
	for _, s := range []obligations.Status{obligations.Satisfied, obligations.Violated, obligations.CantProve, obligations.Unmatched} {
		if !consumerKnows[string(s)] {
			t.Errorf("producer status %q has no consumer constant — checkObligations cannot match it", s)
		}
	}
}
