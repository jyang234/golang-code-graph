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

// RenderBySource formats the per-source decomposition (AnalyzeBySource) as an additive
// block: one line per declared source with its INDEPENDENT verdict, so one source's
// FLOW does not mask another's NO-FLOW / ABSTAIN / FLOW. A pure function of the
// reports, so it is deterministic. Returns "" when there are no sources (nothing to add).
func RenderBySource(reports []SourceReport) string {
	if len(reports) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  by source (independent — one source's FLOW does not mask another):\n")
	for _, sr := range reports {
		switch sr.Report.Verdict {
		case Flow:
			fmt.Fprintf(&b, "    %-8s %s  → %s\n", sr.Report.Verdict, sr.Source, flowTargets(sr.Report.Flows))
		case Abstain:
			fmt.Fprintf(&b, "    %-8s %s  escaped at %s\n", sr.Report.Verdict, sr.Source, strings.Join(sr.Report.EscapeSites, ", "))
		default: // NoFlow
			fmt.Fprintf(&b, "    %-8s %s\n", sr.Report.Verdict, sr.Source)
		}
	}
	return b.String()
}

// RenderBySink formats the per-sink decomposition (AnalyzeBySink) as an additive block:
// one line per declared sink with its INDEPENDENT verdict — which sink actually receives
// source data, and from where. A pure function of the reports, so it is deterministic.
// Returns "" when there are no sinks (nothing to add).
func RenderBySink(reports []SinkReport) string {
	if len(reports) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  by sink (independent — which sink receives source data):\n")
	for _, sr := range reports {
		switch sr.Report.Verdict {
		case Flow:
			fmt.Fprintf(&b, "    %-8s %s  ← %s\n", sr.Report.Verdict, sr.Sink, flowSites(sr.Report.Flows))
		case Abstain:
			fmt.Fprintf(&b, "    %-8s %s  (taint escaped a modeled construct — cannot prove no-flow)\n", sr.Report.Verdict, sr.Sink)
		default: // NoFlow
			fmt.Fprintf(&b, "    %-8s %s\n", sr.Report.Verdict, sr.Sink)
		}
	}
	return b.String()
}

// flowSites renders the first-party sites where a sink is reached, joined — the "which
// callers leak into this sink" detail the aggregate verdict collapses.
func flowSites(flows []Finding) string {
	parts := make([]string, 0, len(flows))
	for _, f := range flows {
		parts = append(parts, f.Site)
	}
	return strings.Join(parts, "; ")
}

// flowTargets renders a source's FLOW findings as "sink at site" entries, joined — the
// "which field reaches which sink where" detail the aggregate verdict collapses.
func flowTargets(flows []Finding) string {
	parts := make([]string, 0, len(flows))
	for _, f := range flows {
		parts = append(parts, fmt.Sprintf("%s at %s", f.Sink, f.Site))
	}
	return strings.Join(parts, "; ")
}
