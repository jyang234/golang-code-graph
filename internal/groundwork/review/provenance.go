package review

import (
	"fmt"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// provenanceCaveats returns the call-graph substrate notes to record on a verdict
// computed over a base/branch pair. The branch's caveats are the provenance of
// the code under review; when the two sides were built on different algorithms a
// synthesized caveat discloses the mismatch, because a delta computed across
// substrates can move for reasons that are the analyzer's, not the code's. Nil
// when neither side recorded provenance (graphs from a pre-provenance flowmap).
func provenanceCaveats(base, branch *graph.Graph) []string {
	var out []string
	if base.Algo != "" && branch.Algo != "" && base.Algo != branch.Algo {
		out = append(out, fmt.Sprintf("base graph built on %s, branch on %s — substrate differs; a delta may be the analyzer's, not the code's", base.Algo, branch.Algo))
	}
	out = append(out, branch.Caveats...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// renderProvenance is the review/gate substrate disclosure — graph.ProvenanceLine
// with the base/branch mismatch caveat already folded into caveats by the caller.
func renderProvenance(algo string, caveats []string) string {
	return graph.ProvenanceLine(algo, caveats)
}
