package review

import (
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/impeach"
)

// a clean, static-passing base==branch pair, so the ONLY thing that can flip Pass
// in these tests is the behavioral impeachment input.
func cleanPair() (*graph.Graph, *graph.Graph) {
	g := &graph.Graph{Nodes: []graph.Node{{FQN: "svc.handler", Sig: "func()"}}}
	return g, g
}

func violatedBlocker() impeach.GateFinding {
	return impeach.GateFinding{
		Effect: "db DELETE ledger", Flow: "POST /x", Entry: "HTTP POST /x",
		Verdict: impeach.VerdictViolated, Rule: "no-delete-from-web",
		Reason: "observed reaching",
	}
}

// TestGateImpeachmentDisclosesButDoesNotBlockWithoutOptIn is observe-first (§10):
// with no impeachment_gate opt-in, a behaviorally-confirmed breach is DISCLOSED in
// the result and the human report, but the gate still PASSES — it blocks only once
// ratified.
func TestGateImpeachmentDisclosesButDoesNotBlockWithoutOptIn(t *testing.T) {
	base, branch := cleanPair()
	p := &policy.Policy{Service: "svc", Version: 1} // no ImpeachmentGate
	g := Gate(p, base, branch, nil, WithImpeachment([]impeach.GateFinding{violatedBlocker()}))

	if !g.Pass {
		t.Fatalf("a breach must not block without the opt-in (observe-first); got pass=%v", g.Pass)
	}
	if len(g.ImpeachmentBreaches) != 1 {
		t.Fatalf("the breach must be disclosed even when not gating; got %+v", g.ImpeachmentBreaches)
	}
	if !strings.Contains(g.Render(), "behavioral impeachment") {
		t.Errorf("the human report must disclose the breach on PASS; got:\n%s", g.Render())
	}
}

// TestGateImpeachmentBlocksWithOptIn: once impeachment_gate.gate is ratified, the
// SAME breach fails the gate (§9 — the gate returns BLOCK).
func TestGateImpeachmentBlocksWithOptIn(t *testing.T) {
	base, branch := cleanPair()
	p := &policy.Policy{Service: "svc", Version: 1, ImpeachmentGate: &policy.ImpeachmentGate{Gate: true}}
	g := Gate(p, base, branch, nil, WithImpeachment([]impeach.GateFinding{violatedBlocker()}))

	if g.Pass {
		t.Fatal("a ratified impeachment_gate must block on a behaviorally-confirmed breach")
	}
	if !strings.Contains(g.Render(), "BLOCK") {
		t.Errorf("the verdict must be BLOCK; got:\n%s", g.Render())
	}
}

// TestGateNoImpeachmentInputIsByteIdentical: the default static gate (no
// WithImpeachment) carries no breaches and an unchanged digest — behavioral
// integration never perturbs an existing verify.
func TestGateNoImpeachmentInputIsByteIdentical(t *testing.T) {
	base, branch := cleanPair()
	p := &policy.Policy{Service: "svc", Version: 1}
	plain := Gate(p, base, branch, nil)
	withEmpty := Gate(p, base, branch, nil, WithImpeachment(nil))
	if len(plain.ImpeachmentBreaches) != 0 {
		t.Errorf("default gate carries breaches: %+v", plain.ImpeachmentBreaches)
	}
	if plain.Digest != withEmpty.Digest {
		t.Errorf("an empty impeachment input changed the digest: %q vs %q", plain.Digest, withEmpty.Digest)
	}
}

// TestGateImpeachmentOptInOnlyGatesWithBreaches: the opt-in alone, with no breach,
// passes — the opt-in is a permission to block, not a block itself.
func TestGateImpeachmentOptInOnlyGatesWithBreaches(t *testing.T) {
	base, branch := cleanPair()
	p := &policy.Policy{Service: "svc", Version: 1, ImpeachmentGate: &policy.ImpeachmentGate{Gate: true}}
	if g := Gate(p, base, branch, nil); !g.Pass {
		t.Errorf("opt-in with no breach must pass; got %+v", g)
	}
}

// stampedAlgoPair mirrors the REAL gating path: a CI-built graph that records its
// substrate (Algo). base==branch keeps the static gate clean, isolating the
// behavioral input as before. (The disclosure surfaces on an algo-less graph too —
// ProvenanceLine no longer drops caveats when the substrate is unrecorded; that
// regression is guarded at the ProvenanceLine level in graph_test.go.)
func stampedAlgoPair() (*graph.Graph, *graph.Graph) {
	g := &graph.Graph{Algo: "rta", Nodes: []graph.Node{{FQN: "svc.handler", Sig: "func()"}}}
	return g, g
}

// TestGateImpeachmentDisclosesCommittedCorpusIdentity: a present behavioral
// impeachment cleared the code-identity rung by injection — the committed corpus is
// stamped with the gated graph's own stamp (§14-E), so the rung verifies only that
// the graph is stamped, not that the corpus came from the gated code. The gate must
// DISCLOSE that corpus freshness is an operational assumption: structurally in
// Caveats (so a CI consumer of --json sees it, baked into the digest) AND in the
// human render, never silently trusting the corpus while advertising a mechanical
// check.
func TestGateImpeachmentDisclosesCommittedCorpusIdentity(t *testing.T) {
	base, branch := stampedAlgoPair()
	p := &policy.Policy{Service: "svc", Version: 1}
	g := Gate(p, base, branch, nil, WithImpeachment([]impeach.GateFinding{violatedBlocker()}))

	if !hasCaveat(g.Caveats, committedCorpusIdentityCaveat) {
		t.Errorf("a present impeachment must disclose the committed-corpus identity assumption in caveats; got %q", g.Caveats)
	}
	if !strings.Contains(g.Render(), "re-captured") {
		t.Errorf("the human render must surface the committed-corpus freshness disclosure; got:\n%s", g.Render())
	}

	// Absent when there is no impeachment: the disclosure qualifies a finding, so its
	// absence keeps the static gate byte-identical — no stray caveat enters the digest.
	clean := Gate(p, base, branch, nil)
	if hasCaveat(clean.Caveats, committedCorpusIdentityCaveat) {
		t.Errorf("no impeachment must carry no committed-corpus caveat; got %q", clean.Caveats)
	}
	if clean.Digest != Gate(p, base, branch, nil, WithImpeachment(nil)).Digest {
		t.Error("WithImpeachment(nil) must stay byte-identical to the static gate (no disclosure caveat)")
	}
}

func hasCaveat(cs []string, want string) bool {
	for _, c := range cs {
		if c == want {
			return true
		}
	}
	return false
}
