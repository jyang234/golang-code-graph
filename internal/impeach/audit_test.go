package impeach

import (
	"os"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

const (
	loansvcGraph = "../../testdata/groundwork/goldens/loansvc.graph.json"
	loansvcTrace = "../../flow/testdata/flows/post_loan_application.golden.json"
)

func loadIndex(t *testing.T, path string) *graph.Index {
	t.Helper()
	g, err := graph.LoadFile(path)
	if err != nil {
		t.Fatalf("load graph %s: %v", path, err)
	}
	return graph.NewIndex(g)
}

func loadTrace(t *testing.T, path string) *ir.CanonicalTrace {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace %s: %v", path, err)
	}
	tr, err := ir.Load(b)
	if err != nil {
		t.Fatalf("load trace %s: %v", path, err)
	}
	return tr
}

// candidateFor returns the single candidate with the given effect, failing if it
// is absent or duplicated. The impeachsvc missed route now impeaches TWO effects
// (db DELETE ledger + db DELETE audit_log), so a test that reasons about one
// specific witness selects it by effect rather than positional index — robust to
// the deterministic sort order, which is itself asserted separately.
func candidateFor(t *testing.T, r Report, effect string) Witness {
	t.Helper()
	var found []Witness
	for _, w := range r.Candidates {
		if w.Effect == effect {
			found = append(found, w)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one candidate for effect %q, got %d: %+v", effect, len(found), r.Candidates)
	}
	return found[0]
}

// effectsOf is the candidate effect sequence in report order — the determinism
// surface a multi-candidate corpus exercises (the witness sort must be a pure,
// order-independent function of intrinsic data).
func effectsOf(r Report) []string {
	out := make([]string, len(r.Candidates))
	for i, w := range r.Candidates {
		out[i] = w.Effect
	}
	return out
}

// TestAuditDeterministic pins the P0 cross-cutting requirement (§10): the report
// is a pure function of (graph, corpus), byte-identical across runs and
// independent of trace arrival order. The digest is the mechanical witness.
func TestAuditDeterministic(t *testing.T) {
	ix := loadIndex(t, loansvcGraph)
	tr := loadTrace(t, loansvcTrace)

	a := Audit("loansvc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	b := Audit("loansvc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if a.Digest == "" {
		t.Fatal("empty digest")
	}
	if a.Digest != b.Digest {
		t.Errorf("non-deterministic digest: %s != %s", a.Digest, b.Digest)
	}

	// A duplicated/reordered corpus must not perturb the join: the same effect
	// observed twice collapses, and slice order does not reach the output.
	dup := Audit("loansvc", ix, []*ir.CanonicalTrace{tr, tr}, Provenance{})
	if dup.Digest != a.Digest {
		t.Errorf("duplicate trace perturbed the report: %s != %s", dup.Digest, a.Digest)
	}
}

// TestAuditProbeLoansvc is the Phase-0 go/no-go probe (§10): run the join on a
// real (graph, corpus) pair and confirm it produces no FALSE impeachments on a
// sound graph. The corpus observes `db SELECT applicants`, whose only static
// emitter (store.Loans.SelectApplicant) is reachable solely through a disclosed
// `severed-closure` frontier marker (origination.Evaluator.Evaluate$1) — the
// RECLAIMED-LIVE cell, where static abstains — so it must be EXCLUDED from
// candidates, not laundered into a false "unreachable" negative. A clean probe
// here is the healthy signal: the join fires only on genuine contradictions
// (proven by the synthetic tests below).
func TestAuditProbeLoansvc(t *testing.T) {
	ix := loadIndex(t, loansvcGraph)
	tr := loadTrace(t, loansvcTrace)
	r := Audit("loansvc", ix, []*ir.CanonicalTrace{tr}, Provenance{})

	for _, w := range r.Candidates {
		if w.Effect == "db SELECT applicants" {
			t.Errorf("frontier-covered effect was impeached: %+v", w)
		}
	}
	if len(r.Candidates) != 0 {
		t.Errorf("probe found %d candidate(s) on a sound graph; want 0:\n%+v", len(r.Candidates), r.Candidates)
	}

	// The green is scoped to its evidence: reachable-but-unobserved effects are
	// disclosed as coverage gaps (here, e.g. the loan.declined publish the happy
	// path never drives), never silently passed as "nothing happened".
	t.Logf("probe loansvc: %d candidates, %d coverage gaps, corpus=%s",
		len(r.Candidates), len(r.CoverageGaps), short(r.CorpusDigest))
	t.Logf("caveats: %v", r.Caveats)
	t.Logf("coverage gaps: %v", r.CoverageGaps)
}

// TestAuditFiresOnRealGraphForUnmodeledEffect guards against the frontier
// seeding OVER-suppressing: on the real loansvc graph (which carries genuine
// severed-closure + dynamic-bus disclosures), an effect the graph models no
// emitter for AND no blind spot covers — a DELETE on a table loansvc never
// touches — must still surface as an ABSENT candidate. A join that suppressed
// even this would be a dead detector.
func TestAuditFiresOnRealGraphForUnmodeledEffect(t *testing.T) {
	ix := loadIndex(t, loansvcGraph)
	tr := &ir.CanonicalTrace{Flow: "POST /loan-application", Service: "loansvc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /loan-application", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE shadow_ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("loansvc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 || r.Candidates[0].Effect != "db DELETE shadow_ledger" {
		t.Fatalf("want 1 ABSENT candidate db DELETE shadow_ledger, got %+v", r.Candidates)
	}
	if r.Candidates[0].Claim.Reachability != ReachAbsent {
		t.Errorf("Reachability = %q, want %q", r.Candidates[0].Claim.Reachability, ReachAbsent)
	}
}

// TestAuditFlagsAbsentEmitter proves the join FIRES when an effect is observed
// that the graph models NO emitter for, and no blind spot covers it: a missing
// emitter/root, the strongest impeachment shape.
func TestAuditFlagsAbsentEmitter(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes:       []graph.Node{{FQN: "svc.handler"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	w := r.Candidates[0]
	if w.Effect != "db DELETE ledger" || w.Claim.Reachability != ReachAbsent {
		t.Errorf("unexpected witness: %+v", w)
	}
	if w.Observed.Op != "DB postgres DELETE ledger" || w.Observed.Entry != "HTTP POST /x" {
		t.Errorf("observation lost enrichment/entry: %+v", w.Observed)
	}
	// With no caller-supplied provenance the candidate is a real contradiction but
	// its code identity is unestablished, so the ladder caps it at VERSION-SKEW —
	// the fail-closed Phase-1 outcome on a corpus that carries no commit stamp.
	if w.Verdict != DowngradeVersionSkew {
		t.Errorf("Verdict = %q, want %q (no provenance ⇒ code-identity unestablished)", w.Verdict, DowngradeVersionSkew)
	}
}

// TestAuditFlagsNamedButUnreachable proves the join fires when the graph DOES
// model the emitter but no entrypoint reaches it (a severed effect) — and that
// the right Reachability is recorded, distinguishing it from the absent case.
func TestAuditFlagsNamedButUnreachable(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.handler"}, {FQN: "svc.orphan"}},
		Edges: []graph.Edge{
			{From: "svc.orphan", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	if got := r.Candidates[0].Claim.Reachability; got != ReachUnreachable {
		t.Errorf("Reachability = %q, want %q", got, ReachUnreachable)
	}
}

// TestAuditNoCandidateWhenReachable is the negative control: an effect whose
// emitter IS reachable from the entrypoint is CONFIRMED-LIVE, never a candidate.
func TestAuditNoCandidateWhenReachable(t *testing.T) {
	ix := graph.NewIndex(&graph.Graph{
		Nodes: []graph.Node{{FQN: "svc.handler"}, {FQN: "svc.store"}},
		Edges: []graph.Edge{
			{From: "svc.handler", To: "svc.store"},
			{From: "svc.store", To: "boundary:db DELETE ledger", Boundary: "outbound-sync"},
		},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
	tr := &ir.CanonicalTrace{Flow: "POST /x", Service: "svc", Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE ledger", Kind: ir.KindClient},
		}}},
	}}
	r := Audit("svc", ix, []*ir.CanonicalTrace{tr}, Provenance{})
	if len(r.Candidates) != 0 {
		t.Errorf("reachable effect impeached: %+v", r.Candidates)
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
