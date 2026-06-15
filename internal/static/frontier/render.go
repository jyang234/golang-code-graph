package frontier

import (
	"fmt"
	"strings"
)

// Render is the human view of a Report: the headline ratios, the per-bin counts,
// and the reclaimable seams a reader would act on first. The --json output carries
// the full marker list; this is the at-a-glance summary.
func Render(name, algo string, r *Report) string {
	var b strings.Builder
	if algo == "" {
		algo = "unrecorded"
	}
	fmt.Fprintf(&b, "frontier: %s  (algo %s)\n", name, algo)
	fmt.Fprintf(&b, "  entrypoints: %d   starved: %d (%.0f%% attribution loss)\n",
		r.Entrypoints, r.StarvedEntrypoints, 100*r.AttributionLoss)
	if r.Entrypoints > 0 {
		// Be honest about the detector's coverage: starvation is confirmed only for
		// the oapi-codegen strict-server shape, so a low/zero attribution loss is
		// "no CONFIRMED seam", not a proof of no severance (other dispatch frameworks
		// are not yet recognized). See docs/design/frontier-instrumentation-plan.md §3.
		b.WriteString("    (attribution loss confirms the oapi strict-server shape only; " +
			"0% means no CONFIRMED seam, not no severance)\n")
	}
	fmt.Fprintf(&b, "  markers: %d   reclaimable (B): %d (%.0f%%)\n",
		len(r.Markers), r.Counts[BinB], 100*r.ReclaimableShare)
	fmt.Fprintf(&b, "    A  truly dynamic       : %d\n", r.Counts[BinA])
	fmt.Fprintf(&b, "    B  reclaimable seam     : %d\n", r.Counts[BinB])
	fmt.Fprintf(&b, "    B2 opaque, make const   : %d\n", r.Counts[BinB2])
	fmt.Fprintf(&b, "    C  over-approximation   : %d\n", r.Counts[BinC])

	var seams []Marker
	for _, m := range r.Markers {
		if m.Bin == BinB {
			seams = append(seams, m)
		}
	}
	if len(seams) > 0 {
		b.WriteString("  reclaimable seams:\n")
		for _, m := range seams {
			fmt.Fprintf(&b, "    - %-20s %s\n", m.Kind, short(m.Site))
			if m.ReclaimerHint != "" {
				fmt.Fprintf(&b, "        reclaim: %s\n", m.ReclaimerHint)
			}
		}
	}
	return b.String()
}
