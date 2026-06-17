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
