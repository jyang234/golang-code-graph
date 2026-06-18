package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// liveTrace is a captured trace carrying a code-identity Stamp (the live/production
// path, where the corpus self-describes its deploy) plus one unmodeled DB DELETE.
func liveTrace(table, svc, stamp string) *ir.CanonicalTrace {
	return &ir.CanonicalTrace{Flow: "POST /x", Service: svc, Stamp: stamp, Root: &ir.CanonicalSpan{
		Op: "HTTP POST /x", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "DB postgres DELETE " + table, Kind: ir.KindClient},
		}}},
	}}
}

// stampedAbsentGraph models an entrypoint with NO emitter for the DB write, so an
// observed write is an ABSENT candidate; stamped with the given code identity.
func stampedAbsentGraph(stamp string) *graph.Index {
	return graph.NewIndex(&graph.Graph{
		Stamp:       stamp,
		Nodes:       []graph.Node{{FQN: "svc.handler"}},
		Entrypoints: []graph.Entrypoint{{Kind: "http", Name: "POST /x", Fn: "svc.handler"}},
	})
}

// TestAuditLivePathPromotesFromTraceStamp is the capture-side stamp end to end on
// the LIVE path: the corpus carries its own deploy commit (no injected
// Provenance.TraceIdentity), it matches the stamped graph, and the candidate
// promotes to IMPEACHMENT. This is the §12.1 hinge — a Phase-1 impeachment made
// meaningful by a real captured code identity rather than a caller assertion.
func TestAuditLivePathPromotesFromTraceStamp(t *testing.T) {
	ix := stampedAbsentGraph("c1")
	traces := []*ir.CanonicalTrace{liveTrace("ledger", "svc", "c1")}
	r := Audit("svc", ix, traces, Provenance{Capture: CaptureProduction})

	if r.TraceIdentity != "c1" {
		t.Errorf("report TraceIdentity = %q, want the trace-carried %q", r.TraceIdentity, "c1")
	}
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	if v := r.Candidates[0].Verdict; v != VerdictImpeachment {
		t.Errorf("Verdict = %q, want %q; ladder = %+v", v, VerdictImpeachment, r.Candidates[0].Rungs)
	}
}

// TestAuditLivePathSkewDowngrades: a trace whose deploy commit differs from the
// graph's stamp is VERSION-SKEW — the audited graph is not the code that ran, so
// its negative cannot be impeached by that capture (fail closed, §4 rung 2).
func TestAuditLivePathSkewDowngrades(t *testing.T) {
	ix := stampedAbsentGraph("c1")
	traces := []*ir.CanonicalTrace{liveTrace("ledger", "svc", "c2")}
	r := Audit("svc", ix, traces, Provenance{Capture: CaptureProduction})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(r.Candidates))
	}
	if v := r.Candidates[0].Verdict; v != DowngradeVersionSkew {
		t.Errorf("Verdict = %q, want %q", v, DowngradeVersionSkew)
	}
}

// TestAuditMixedCorpusFailsClosed: a corpus mixing two deploys establishes no
// single identity, so every candidate downgrades to VERSION-SKEW rather than
// impeaching against a graph that matches only one of them.
func TestAuditMixedCorpusFailsClosed(t *testing.T) {
	ix := stampedAbsentGraph("c1")
	traces := []*ir.CanonicalTrace{
		liveTrace("ledger_a", "svc", "c1"),
		liveTrace("ledger_b", "svc", "c2"),
	}
	r := Audit("svc", ix, traces, Provenance{Capture: CaptureProduction})
	if len(r.Candidates) == 0 {
		t.Fatal("want candidates to classify")
	}
	for _, w := range r.Candidates {
		if w.Verdict != DowngradeVersionSkew {
			t.Errorf("mixed corpus: %s = %q, want %q", w.Effect, w.Verdict, DowngradeVersionSkew)
		}
	}
}

// TestAuditInjectedIdentityContradictsCorpus: an injected Provenance.TraceIdentity
// that disagrees with the stamp the traces actually carry is a caller error, and
// fails closed to unestablished (VERSION-SKEW) rather than letting the assertion
// override the evidence.
func TestAuditInjectedIdentityContradictsCorpus(t *testing.T) {
	ix := stampedAbsentGraph("c1")
	traces := []*ir.CanonicalTrace{liveTrace("ledger", "svc", "c1")}
	r := Audit("svc", ix, traces, Provenance{TraceIdentity: "c2", Capture: CaptureProduction})
	if len(r.Candidates) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(r.Candidates))
	}
	if v := r.Candidates[0].Verdict; v != DowngradeVersionSkew {
		t.Errorf("Verdict = %q, want %q (injected identity contradicts the corpus)", v, DowngradeVersionSkew)
	}
}

// TestCorpusDigestExcludesStamp guards the regression finding-1 fixed: the corpus
// digest must NOT churn on the run-varying deploy stamp. Two captures of one flow
// on different deploys are the SAME canonical trace (golden equality zeroes Stamp),
// so they must produce the SAME CorpusDigest — the deploy identity is carried
// separately as Report.TraceIdentity.
func TestCorpusDigestExcludesStamp(t *testing.T) {
	ix := stampedAbsentGraph("c1")
	c1 := Audit("svc", ix, []*ir.CanonicalTrace{liveTrace("ledger", "svc", "c1")}, Provenance{Capture: CaptureProduction})
	c2ix := stampedAbsentGraph("c2")
	c2 := Audit("svc", c2ix, []*ir.CanonicalTrace{liveTrace("ledger", "svc", "c2")}, Provenance{Capture: CaptureProduction})
	if c1.CorpusDigest != c2.CorpusDigest {
		t.Errorf("CorpusDigest churned on the deploy stamp: %s != %s", c1.CorpusDigest, c2.CorpusDigest)
	}
	// The deploy identity is still distinguished, in TraceIdentity, so the full
	// report digests legitimately differ.
	if c1.TraceIdentity == c2.TraceIdentity {
		t.Error("TraceIdentity should reflect the differing deploy")
	}
	if c1.Digest == c2.Digest {
		t.Error("Report.Digest should differ when TraceIdentity differs")
	}
}

// TestCorpusDigestIncludesProvenance pins the deliberate DIVERGENCE from golden
// equality (golden.canonicalBytes zeroes the capture grade for behavior-purity): the
// audit's corpus identity MUST fold the grade IN, because a production-graded corpus
// and an integration-graded corpus of the same flow license different verdicts (only
// the trusted grades promote a gating impeachment), so they must digest DIFFERENTLY.
// This is the exact opposite treatment from the snapshot gate, and the pair
// (this + TestCorpusDigestExcludesStamp) pins both halves: the run-varying Stamp is
// excluded from corpus identity, the trust-bearing grade is included.
func TestCorpusDigestIncludesProvenance(t *testing.T) {
	mk := func(grade string) []*ir.CanonicalTrace {
		return []*ir.CanonicalTrace{{Flow: "POST /x", Service: "svc", Provenance: grade}}
	}
	prod := corpusDigest(mk(CaptureProduction))
	integ := corpusDigest(mk(CaptureIntegration))
	if prod == integ {
		t.Error("corpusDigest did not fold the capture grade into corpus identity; a production and an integration corpus of one flow collapsed to the same digest")
	}
	// Still a pure function of the grade (reproducible run-to-run).
	if prod != corpusDigest(mk(CaptureProduction)) {
		t.Error("corpusDigest not reproducible for a fixed grade")
	}
}

func TestCorpusIdentity(t *testing.T) {
	mk := func(stamp string) *ir.CanonicalTrace { return &ir.CanonicalTrace{Stamp: stamp} }
	cases := []struct {
		name   string
		traces []*ir.CanonicalTrace
		want   string
	}{
		{"agree", []*ir.CanonicalTrace{mk("c1"), mk("c1")}, "c1"},
		{"disagree", []*ir.CanonicalTrace{mk("c1"), mk("c2")}, ""},
		{"one-stampless", []*ir.CanonicalTrace{mk("c1"), mk("")}, ""},
		{"all-stampless", []*ir.CanonicalTrace{mk(""), mk("")}, ""},
		{"empty", nil, ""},
		{"nil-skipped", []*ir.CanonicalTrace{nil, mk("c1")}, "c1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := corpusIdentity(c.traces); got != c.want {
				t.Errorf("corpusIdentity = %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveIdentity(t *testing.T) {
	mk := func(stamp string) []*ir.CanonicalTrace { return []*ir.CanonicalTrace{{Stamp: stamp}} }
	cases := []struct {
		name   string
		traces []*ir.CanonicalTrace
		inject string
		want   string
	}{
		{"derived-only", mk("c1"), "", "c1"},
		{"injected-only", mk(""), "c1", "c1"},
		{"both-agree", mk("c1"), "c1", "c1"},
		{"contradiction", mk("c1"), "c2", ""},
		{"neither", mk(""), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveIdentity(c.traces, Provenance{TraceIdentity: c.inject}); got != c.want {
				t.Errorf("resolveIdentity = %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveCaptureProvenance pins the §12.6 reconciliation — the same shape as
// resolveIdentity, applied to the producer-set capture grade. A contradiction
// (caller asserts a grade the corpus does not carry) fails CLOSED to "", which caps
// the capture-fidelity rung at CAPTURE-UNTRUSTED: the audit can never assert a grade
// the capture itself contradicts.
func TestResolveCaptureProvenance(t *testing.T) {
	mk := func(grade string) []*ir.CanonicalTrace { return []*ir.CanonicalTrace{{Provenance: grade}} }
	cases := []struct {
		name   string
		traces []*ir.CanonicalTrace
		caller string
		want   string
	}{
		{"corpus-self-describes", mk(CaptureIntegration), "", CaptureIntegration},
		{"caller-asserts-ungraded-corpus", mk(""), CaptureProduction, CaptureProduction},
		{"both-agree", mk(CaptureIntegration), CaptureIntegration, CaptureIntegration},
		{"contradiction-fails-closed", mk(CaptureIntegration), CaptureProduction, ""},
		{"neither", mk(""), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveCaptureProvenance(c.traces, Provenance{Capture: c.caller}); got != c.want {
				t.Errorf("resolveCaptureProvenance = %q, want %q", got, c.want)
			}
		})
	}
}

// TestImpeachsvcLivePathPromotes is the live-path companion to the injected-
// provenance promotion test: the real impeachsvc graph stamped with a commit, and
// the real captured traces carrying that SAME commit as their Stamp, yield an
// IMPEACHMENT with no injected Provenance.TraceIdentity — the corpus self-describes
// the code it ran.
func TestImpeachsvcLivePathPromotes(t *testing.T) {
	g, err := graph.LoadFile(impeachsvcGraph)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	const commit = "deadbeefcafe"
	g.Stamp = commit
	ix := graph.NewIndex(g)

	purge := loadTrace(t, impeachTraceAdminPurge)
	create := loadTrace(t, impeachTraceLoanCreate)
	purge.Stamp = commit
	create.Stamp = commit

	// No caller provenance at all: the live corpus self-describes BOTH its code
	// identity (the Stamp set above) AND its capture grade (the goldens carry
	// "integration", §12.6) — the audit asserts nothing.
	r := Audit("impeachsvc", ix, []*ir.CanonicalTrace{purge, create}, Provenance{})
	if len(r.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(r.Candidates), r.Candidates)
	}
	// Both missed-route DELETEs promote off the trace's self-described identity+grade.
	for _, w := range r.Candidates {
		if w.Verdict != VerdictImpeachment {
			t.Errorf("Verdict = %q for %q, want %q; ladder = %+v", w.Verdict, w.Effect, VerdictImpeachment, w.Rungs)
		}
	}
}
