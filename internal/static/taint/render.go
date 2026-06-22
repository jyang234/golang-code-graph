package taint

import (
	"fmt"
	"strings"
)

// Render formats a Report as a human-readable disclosure: the verdict, the
// could-flow findings, and — when the verdict is ABSTAIN — the escape sites that
// blocked a no-flow proof. A pure function of the Report, so it is deterministic.
func Render(name string, r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "taint: %s\n", name)
	fmt.Fprintf(&b, "  verdict: %s\n", r.Verdict)
	fmt.Fprintf(&b, "  sources seeded: %d\n", r.Sources)
	switch r.Verdict {
	case Flow:
		fmt.Fprintf(&b, "  FLOW (%d) — source data reaches a sink (candidates to verify):\n", len(r.Flows))
		for _, f := range r.Flows {
			fmt.Fprintf(&b, "    - %s  at %s\n", f.Sink, f.Site)
		}
	case NoFlow:
		fmt.Fprintf(&b, "  NO-FLOW — proven: no declared source reaches a declared sink (forward cone complete)\n")
	case Abstain:
		fmt.Fprintf(&b, "  ABSTAIN — cannot prove no-flow: taint escaped a modeled construct (map/interface/channel/closure/external call)\n")
		if len(r.EscapeSites) > 0 {
			fmt.Fprintf(&b, "  escape sites: %s\n", strings.Join(r.EscapeSites, ", "))
		}
	}
	return b.String()
}
