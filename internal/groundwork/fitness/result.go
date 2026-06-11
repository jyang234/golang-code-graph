package fitness

import "sort"

// Severity ranks a finding. Violations fail the gate; Cautions are surfaced but
// do not fail it — they are the legible form of the graph abstaining (a
// must-not-reach rule it could not prove because the frontier is blind), so the
// tool's silence is never mistaken for a clean pass.
type Severity int

const (
	// Caution is advisory: surfaced, exit 0. The graph cannot make a sound claim.
	Caution Severity = iota
	// Violation fails the gate (exit non-zero): a declared invariant is broken.
	Violation
)

func (s Severity) String() string {
	if s == Violation {
		return "violation"
	}
	return "caution"
}

// Finding is one result of evaluating a rule. From/To name the exact edge when
// the finding is about one (layering, a reachable path); they are empty for
// rule-level findings (a budget overflow names the route in Summary). Detail is
// presentation only — extra evidence for the reader (e.g. a witness path) that
// is never part of a finding's identity, so re-derived prose cannot make an old
// finding look new in a base-vs-branch diff.
type Finding struct {
	Rule     string
	Severity Severity
	Summary  string
	From     string
	To       string
	Detail   string
}

// Result is the full set of findings from evaluating a policy against a graph.
type Result struct {
	Findings []Finding
}

// add appends a finding.
func (r *Result) add(f Finding) { r.Findings = append(r.Findings, f) }

// OK reports whether the gate passes — no Violation-severity findings. Cautions
// do not affect it.
func (r *Result) OK() bool {
	for _, f := range r.Findings {
		if f.Severity == Violation {
			return false
		}
	}
	return true
}

// Violations returns the gate-failing findings.
func (r *Result) Violations() []Finding { return r.bySeverity(Violation) }

// Cautions returns the advisory findings.
func (r *Result) Cautions() []Finding { return r.bySeverity(Caution) }

func (r *Result) bySeverity(s Severity) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == s {
			out = append(out, f)
		}
	}
	return out
}

// sort orders findings deterministically: violations before cautions, then by
// rule, summary, and edge. Called once after all checks run.
func (r *Result) sort() {
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity // Violation (1) before Caution (0)
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Summary != b.Summary {
			return a.Summary < b.Summary
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
}
