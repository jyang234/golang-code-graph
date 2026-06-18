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
	fmt.Fprintf(&b, "  entrypoints: %d   confirmed-severed: %d (%.0f%% attribution loss, a LOWER BOUND)\n",
		r.Entrypoints, r.StarvedEntrypoints, 100*r.AttributionLoss)
	// The third state: routes reaching no effect whose severance we could not
	// confirm. Listed here (the on-demand view) but kept as an aggregate count in
	// the committed graph section. A 0 attribution loss with unconfirmed routes is
	// NOT a proof of no severance — disclose it where a reader will see it.
	if len(r.UnconfirmedRoutes) > 0 {
		fmt.Fprintf(&b, "  unconfirmed (reach no effect, cause unverified): %d\n", len(r.UnconfirmedRoutes))
		for _, fn := range r.UnconfirmedRoutes {
			fmt.Fprintf(&b, "    - %s\n", short(fn))
		}
		b.WriteString("    (no-ops, or seams this classifier does not recognize — attribution_loss is a lower bound)\n")
	}
	fmt.Fprintf(&b, "  markers: %d   reclaimable (B): %d (%.0f%%)\n",
		len(r.Markers), r.Counts[BinB], 100*r.ReclaimableShare)
	fmt.Fprintf(&b, "    A  truly dynamic       : %d\n", r.Counts[BinA])
	fmt.Fprintf(&b, "    B  reclaimable seam     : %d\n", r.Counts[BinB])
	// The B2 line splits on whether the SQL fold ran: after --reclaim-sql the
	// remaining opaque-db markers are the genuine B2b residue (the consumer ask);
	// before it, the count is the undifferentiated union and --reclaim-sql is the
	// first lever to pull (it folds the B2a constant-fragment-builder sub-class).
	if r.SQLFolded {
		fmt.Fprintf(&b, "    B2 opaque, residue      : %d (B2b — --reclaim-sql could not fold these; make them const)\n", r.Counts[BinB2])
	} else {
		fmt.Fprintf(&b, "    B2 opaque, make const   : %d (try --reclaim-sql to fold the B2a builder sub-class)\n", r.Counts[BinB2])
	}
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
