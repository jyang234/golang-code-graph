package fitness

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
)

// The obligsvc golden carries one obligation verdict per shape (path-obligations
// plan §7); groundwork judges them: VIOLATED → violation, CANT-PROVE and
// UNMATCHED → caution, SATISFIED → nothing.
func TestObligationsJudged(t *testing.T) {
	g := loadGraph(t, "obligsvc.graph.json")
	res := Check(&policy.Policy{Service: "obligsvc", Version: 1}, graph.NewIndex(g))

	v := res.Violations()
	if len(v) != 3 {
		t.Fatalf("want 3 obligation violations (Transfer leak, DisburseRacy, DeferredPublish), got %v", v)
	}
	froms := map[string]bool{}
	for _, f := range v {
		if f.Rule != "obligation" {
			t.Errorf("violation rule = %q, want obligation", f.Rule)
		}
		froms[f.From] = true
	}
	for _, want := range []string{
		"example.com/obligsvc/internal/app.Transfer",
		"example.com/obligsvc/internal/app.DisburseRacy",
		"example.com/obligsvc/internal/app.DeferredPublish",
	} {
		if !froms[want] {
			t.Errorf("violations name %v, missing %s", froms, want)
		}
	}

	c := res.Cautions()
	if len(c) != 3 {
		t.Fatalf("want 3 obligation cautions (2x CANT-PROVE, UNMATCHED), got %v", c)
	}
	var inert, cantProve bool
	for _, f := range c {
		if strings.Contains(f.Summary, "inert guardrail") {
			inert = true
		}
		if strings.Contains(f.Summary, "cannot prove") {
			cantProve = true
		}
	}
	if !inert || !cantProve {
		t.Errorf("cautions = %v, want one inert-rule and one cannot-prove", c)
	}
}

// SATISFIED is the proof: it must produce no finding at all.
func TestObligationsSatisfiedIsSilent(t *testing.T) {
	g := loadGraph(t, "obligsvc.graph.json")
	res := Check(&policy.Policy{Service: "obligsvc", Version: 1}, graph.NewIndex(g))
	for _, f := range res.Findings {
		switch f.From {
		case "example.com/obligsvc/internal/app.TransferDefer",
			"example.com/obligsvc/internal/app.Disburse",
			"example.com/obligsvc/internal/app.TransferClosure",
			"example.com/obligsvc/internal/app.TransferAnnotate",
			"example.com/obligsvc/internal/app.TransferConcrete",
			"example.com/obligsvc/internal/app.HoldSem",
			"example.com/obligsvc/internal/app.DeferredPublishAudited":
			t.Errorf("SATISFIED site produced a finding: %v", f)
		}
	}
}

// RF-1: fail closed on vocabulary drift. A status this groundwork does not
// recognize must surface as a caution, never read as a pass.
func TestUnknownObligationStatusIsCaution(t *testing.T) {
	g := loadGraph(t, "obligsvc.graph.json")
	g.Obligations = append(g.Obligations, graph.Obligation{
		Rule: "tx-must-close", Kind: "must-release",
		Fn: "example.com/obligsvc/internal/app.Transfer", Site: "internal/app/app.go:99",
		Status: "CANT-PROVE-OWNERSHIP", // a future producer-side refinement
	})
	res := Check(&policy.Policy{Service: "obligsvc", Version: 1}, graph.NewIndex(g))
	found := false
	for _, c := range res.Cautions() {
		if strings.Contains(c.Summary, `status "CANT-PROVE-OWNERSHIP" is not understood`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown status produced no caution; findings: %v", res.Findings)
	}
}
