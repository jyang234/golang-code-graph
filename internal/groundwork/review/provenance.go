package review

import (
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

// provenanceCaveats returns the call-graph substrate notes to record on a verdict
// computed over a base/branch pair. The branch's caveats are the provenance of
// the code under review; when the two sides were built on different algorithms — or
// by different flowmap builds — a synthesized caveat discloses the mismatch, because
// a delta computed across substrates OR across producer versions can move for reasons
// that are the analyzer's/tool's, not the code's. Nil when neither side recorded
// provenance (graphs from a pre-provenance flowmap).
func provenanceCaveats(policyAlgo string, base, branch *graph.Graph) []string {
	var out []string
	if ac := graph.AlgoMismatchCaveat(base.Algo, branch.Algo); ac != "" {
		out = append(out, ac)
	}
	// Same class as the algo mismatch, one dimension over: a base built by one
	// flowmap build and a branch by another can diff on a pure tool artifact (a
	// relabeled effect, an SSA-order shift) with the same code. Disclose it so the
	// delta is read as a producer artifact, not a code change (R11).
	if tc := graph.ToolMismatchCaveat(base.Tool, branch.Tool); tc != "" {
		out = append(out, tc)
	}
	// A policy proposed on one algorithm but gated against a graph built on
	// another can surface spurious reachability findings (the algorithms differ
	// in precision); flag it so the gate's findings are read correctly (§9).
	if mc := graph.SubstrateMismatchCaveat(policyAlgo, branch.Algo); mc != "" {
		out = append(out, mc)
	}
	out = append(out, branch.Caveats...)
	// When the branch graph was built with `--reclaim`, the verdict was computed
	// over a substrate that includes edges recovered at a dispatch seam; disclose
	// it on the same substrate line so a reclaim-informed gate is auditable (R9).
	if rc := branch.ReclaimCaveat(); rc != "" {
		out = append(out, rc)
	}
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
