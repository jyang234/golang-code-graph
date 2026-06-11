package impact

import (
	"os"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/capture"

	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/ingest"
	"github.com/jyang234/golang-code-graph/internal/otlpjson"
)

// The triage effectiveness drills (incident-triage plan §7, E1–E3), run as
// committed tests against the loansvc dogfood fixture. The plan's keep/kill
// thresholds are the assertions, so triage quality is a RATCHET: a refactor
// that erodes recall or scoping power fails the suite, and the measured
// numbers print under -v for the drill record (docs/groundwork/drills.md).
//
// E4 (the with/without-MCP agent comparison) needs live agent sessions and is
// deliberately not simulated here — see the drill record for its design.

// loansvcFQN abbreviations for the ground-truth labels.
const (
	lsScore      = "(*example.com/loansvc/internal/client.Bureau).Score"
	lsCharge     = "(*example.com/loansvc/internal/client.Gateway).Charge"
	lsLedger     = "(*example.com/loansvc/internal/store.Loans).InsertLedger"
	lsMarkPaid   = "(*example.com/loansvc/internal/store.Loans).MarkPaid"
	lsEvaluate   = "(*example.com/loansvc/internal/origination.Evaluator).Evaluate"
	lsNotify     = "(*example.com/loansvc/internal/origination.Evaluator).notify"
	lsRun        = "example.com/loansvc.run"
	lsSelectAppl = "(*example.com/loansvc/internal/store.Loans).SelectApplicant"
)

// E1 — staged incident drills. Each scenario is a realistic alert symptom with
// a hand-labeled true culprit (chosen by reading the fixture, not the tool's
// output). Recall: the culprit must be in the suspect set (Matches ∪ Possible)
// — over-approximation should make this ~always true, so a miss is a resolver
// defect, not tuning. Scoping power: the hunt space (suspects ∪ upstream
// callers) as a fraction of the service must stay well under the kill
// threshold "most of the graph" — a card that doesn't narrow isn't pulling
// its weight.
func TestDrillE1RecallAndScopingPower(t *testing.T) {
	ix := index(t, "loansvc.graph.json")
	total := len(ix.Nodes())

	scenarios := []struct {
		name    string
		resolve func() Resolution
		culprit string
	}{
		{"peer credit-bureau timing out",
			func() Resolution { return ResolvePeer(ix, "credit-bureau") }, lsScore},
		{"peer payment-gw returning 502",
			func() Resolution { return ResolvePeer(ix, "payment-gw") }, lsCharge},
		{"table ledger corrupted",
			func() Resolution { return ResolveTable(ix, "ledger") }, lsLedger},
		{"table loans has bad rows",
			func() Resolution { return ResolveTable(ix, "loans") }, lsMarkPaid},
		{"event loan.declined never arrives",
			func() Resolution { return ResolveEvent(ix, "loan.declined") }, lsEvaluate},
		{"event with runtime-chosen name missing",
			// Resolves only via the <dynamic> publisher: the flagged-possible path.
			func() Resolution { return ResolveEvent(ix, "loan.flagged") }, lsNotify},
		{"consumed event payment.settled not arriving",
			func() Resolution { return ResolveEvent(ix, "payment.settled") }, lsRun},
		{"panic frame from a Dynatrace stack trace",
			func() Resolution {
				return ResolveFrame(ix, "example.com/loansvc/internal/store.(*Loans).SelectApplicant")
			}, lsSelectAppl},
	}

	var fractions []float64
	for _, sc := range scenarios {
		res := sc.resolve()
		suspects := append(append([]string{}, res.Matches...), res.Possible...)
		if len(suspects) == 0 {
			t.Errorf("%s: symptom resolved to nothing", sc.name)
			continue
		}
		recall := false
		for _, s := range suspects {
			if s == sc.culprit {
				recall = true
			}
		}
		if !recall {
			t.Errorf("%s: true culprit %s missing from suspects %v — resolver defect", sc.name, sc.culprit, suspects)
		}
		card := ForFault(ix, suspects)
		hunt := map[string]bool{}
		for _, fn := range append(append([]string{}, card.Suspects...), card.Callers...) {
			hunt[fn] = true
		}
		frac := float64(len(hunt)) / float64(total)
		fractions = append(fractions, frac)
		t.Logf("E1 %-45s suspects=%d hunt=%d/%d (%.0f%%) recall=%v",
			sc.name, len(suspects), len(hunt), total, frac*100, recall)
	}

	// Kill threshold: "if the median suspect set is most of the graph, the
	// card narrows nothing and the surface is not pulling its weight."
	med := median(fractions)
	t.Logf("E1 median hunt-space fraction: %.0f%% of %d nodes (kill threshold ≥50%%)", med*100, total)
	if med >= 0.5 {
		t.Errorf("median hunt-space fraction %.2f hit the kill threshold: triage is not narrowing", med)
	}
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64{}, xs...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted[len(sorted)/2]
}

// E2 — the graph-to-trace handoff: "graph to narrow, telemetry to locate."
// Stage an incident trace by dropping the payment-gw charge span from the
// committed collector capture (the request "failed" at the charge), run it
// through the real post-hoc pipeline, and verify the divergence the trace
// analysis finds lands INSIDE the suspect set the graph card bounded.
func TestDrillE2TraceHandoff(t *testing.T) {
	f, err := os.Open("../../../testdata/otlp/loansvc.collector.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	spans, err := otlpjson.Decode(f)
	if err != nil {
		t.Fatal(err)
	}

	full := traceEffects(t, spans)

	// The staged incident: every span for the gateway charge is missing.
	var truncated = spans[:0:0]
	for _, s := range spans {
		if strings.Contains(s.Name, "charge") || strings.Contains(s.Name, "payment-gw") {
			continue
		}
		truncated = append(truncated, s)
	}
	observed := traceEffects(t, truncated)

	_, missing := ingest.CompareEffects(full, observed)
	var chargeOp string
	for _, m := range missing {
		if strings.Contains(m, "payment-gw") {
			chargeOp = m
		}
	}
	if chargeOp == "" {
		t.Fatalf("the staged trace divergence was not located; missing=%v", missing)
	}
	t.Logf("E2 trace analysis located the divergence: %q", chargeOp)

	// The handoff property: the function producing the diverged effect is
	// inside the graph card's suspect set for the matching symptom.
	ix := index(t, "loansvc.graph.json")
	res := ResolvePeer(ix, "payment-gw")
	inSuspects := false
	for _, s := range res.Matches {
		if s == lsCharge {
			inSuspects = true
		}
	}
	if !inSuspects {
		t.Errorf("divergence producer %s not in the card's suspect set %v — the handoff broke", lsCharge, res.Matches)
	}
	t.Logf("E2 divergence producer %s ∈ graph suspect set (%d suspects)", lsCharge, len(res.Matches))
}

// traceEffects runs spans through the real post-hoc pipeline (group →
// canonicalize) and returns the boundary-effect op set, sorted.
func traceEffects(t *testing.T, spans []capture.Span) []string {
	t.Helper()
	flows := ingest.WholeFlows(spans)
	if len(flows) == 0 {
		t.Fatal("no flows reconstructed from the trace")
	}
	set := map[string]bool{}
	for _, fl := range flows {
		tr, err := canon.Canonicalize(fl.Flow, nil)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		for _, op := range ingest.BoundaryEffects(tr.Root) {
			set[op] = true
		}
	}
	out := make([]string, 0, len(set))
	for op := range set {
		out = append(out, op)
	}
	return out
}

// E3 — the staleness demonstration: triage with a graph one commit behind a
// routing change mis-scopes, deterministically. This converts the
// graph-per-deploy prerequisite (and the --stamp/--expect pair) from an
// assertion into evidence.
func TestDrillE3StaleGraphMisScopes(t *testing.T) {
	const refund = "(*example.com/loansvc/internal/handler.App).Refund"

	// The deployed commit added a Refund route that charges the gateway.
	stale, _ := graph.LoadFile(goldensDir + "/loansvc.graph.json")
	deployed, _ := graph.LoadFile(goldensDir + "/loansvc.graph.json")
	deployed.Nodes = append(deployed.Nodes, graph.Node{FQN: refund, Sig: "func()", Tier: 1})
	deployed.Edges = append(deployed.Edges, graph.Edge{From: refund, To: lsCharge, Tier: 1})

	symptom := func(g *graph.Graph) []string {
		ix := graph.NewIndex(g)
		return ForFault(ix, ResolvePeer(ix, "payment-gw").Matches).Entrypoints
	}
	staleEntry, liveEntry := symptom(stale), symptom(deployed)

	inLive, inStale := false, false
	for _, e := range liveEntry {
		if e == refund {
			inLive = true
		}
	}
	for _, e := range staleEntry {
		if e == refund {
			inStale = true
		}
	}
	if !inLive {
		t.Fatalf("fixture drift: deployed graph's card should implicate the new route; got %v", liveEntry)
	}
	if inStale {
		t.Fatalf("stale graph implicated a route it cannot know about")
	}
	t.Logf("E3 stale card names %d degraded entrypoint(s), deployed card %d — the new Refund route is invisible to the stale map. This is the mis-scope --stamp/--expect exists to catch.",
		len(staleEntry), len(liveEntry))
}
