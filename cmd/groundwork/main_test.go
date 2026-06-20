package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
)

func TestRunSmoke(t *testing.T) {
	cases := [][]string{
		{"version"},
		{"help"},
		{"policy-check", "../../testdata/groundwork/policies/layeredsvc.json"},
		{"reach", "../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"(*example.com/layeredsvc/internal/handler.Server).UpdateUser"},
		// fitness passes on both fixtures (layeredsvc cleanly, blindsvc with a
		// caution that does not fail the gate).
		{"fitness", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json"},
		// the documented invocation forms, flags trailing the positionals.
		{"exceptions", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json", "--json"},
		{"triage", "--table", "users",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json"},
		{"triage", "../../testdata/groundwork/goldens/loansvc.graph.json",
			"--fail", "--peer", "credit-bureau"},
		{"fitness", "../../testdata/groundwork/policies/blindsvc.json",
			"../../testdata/groundwork/goldens/blindsvc.graph.json"},
		// review of the good branch is STRUCTURALLY-CLEAR (exit 0).
		{"review", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-good.graph.json"},
		// the committed skip artifact verifies authentic against its source graphs.
		{"verify-artifact", "../../testdata/groundwork/goldens/layeredsvc.branch-skip.artifact.json",
			"../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json"},
		// verify passes on the good branch confined to its scope.
		{"verify", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-good.graph.json",
			"--scope", "example.com/layeredsvc/internal/handler,example.com/layeredsvc/internal/app"},
		// chains composes the cross-service surface; it is observational (exit 0).
		{"chains",
			"--service", "loansvc=../../testdata/groundwork/goldens/loansvc.graph.json",
			"--service", "obligsvc=../../testdata/groundwork/goldens/obligsvc.graph.json",
			"--policy", "../../testdata/groundwork/policies/bus-brokers.json"},
	}
	for _, args := range cases {
		if err := run(args); err != nil {
			t.Errorf("run(%v) = %v, want nil", args, err)
		}
	}
}

func TestRunErrors(t *testing.T) {
	cases := [][]string{
		{"bogus"},
		{"reach", "../../testdata/groundwork/goldens/layeredsvc.graph.json", "no.Such.Func"},
		{"reach", "/nonexistent/graph.json", "x"},
		{"policy-check", "/nonexistent/policy.json"},
		{"fitness", "/nonexistent/policy.json", "../../testdata/groundwork/goldens/layeredsvc.graph.json"},
		{"fitness", "../../testdata/groundwork/policies/layeredsvc.json", "/nonexistent/graph.json"},
		// review of the skip branch is BLOCK → non-zero exit.
		{"review", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json"},
		{"review", "p", "b"}, // wrong arg count
		{"verify-artifact", "/nonexistent/artifact.json", "p", "b", "br"},
		// triage demands exactly one symptom flag: a silently-ignored second
		// symptom would mis-scope an incident hunt.
		{"triage", "--frame", "GetUser", "--table", "users",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json"},
		// a symptom that resolves to nothing is an error, not an empty card.
		{"triage", "--table", "no_such_table",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json"},
		// verify blocks on the skip branch (new violation).
		{"verify", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json"},
		// a trailing --scope with no value is a usage error: silently dropping
		// it would run the gate wider than the caller asked for.
		{"verify", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-good.graph.json",
			"--scope"},
		// diff reports a breaking contract change → non-zero.
		{"diff", "../../testdata/groundwork/goldens/layeredsvc.contract.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch.contract.json"},
		{"diff", "/nonexistent/a.json", "/nonexistent/b.json"},
		// chains with no graph is a usage error, not an empty surface.
		{"chains"},
		{"chains", "--service", "missingequals"},
		{"chains", "/nonexistent/graph.json"},
	}
	for _, args := range cases {
		if err := run(args); err == nil {
			t.Errorf("run(%v) = nil, want error", args)
		}
	}
}

// Issue 3: every subcommand answers -h/--help with a clean exit (its own usage),
// not an error. Previously the FlagSet commands (reach/fitness/policy-check) hit
// an empty default usage and the positional-first init read "-h" as <graph.json>
// ("open -h: no such file"). The interception in run() makes them consistent.
func TestRunHelpFlagIsCleanAcrossSubcommands(t *testing.T) {
	for _, cmd := range []string{
		"reach", "triage", "ground", "chains", "fitness", "review", "verify",
		"diff", "verify-artifact", "exceptions", "transcript", "init", "policy-check", "mcp",
	} {
		for _, flag := range []string{"-h", "--help"} {
			if err := run([]string{cmd, flag}); err != nil {
				t.Errorf("run([%q %q]) = %v, want a clean help exit", cmd, flag, err)
			}
		}
	}
}

// Verdict failures and operational failures exit differently (1 vs 2) so CI
// can tell "the change failed the gate" from "the gate failed to run". The
// boundary is the error's type; main maps it to the exit code.
func TestVerdictVsOperationalErrors(t *testing.T) {
	verdicts := [][]string{
		{"fitness", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json"},
		{"review", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json"},
		{"verify", "../../testdata/groundwork/policies/layeredsvc.json",
			"../../testdata/groundwork/goldens/layeredsvc.graph.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch-skip.graph.json"},
		{"diff", "../../testdata/groundwork/goldens/layeredsvc.contract.json",
			"../../testdata/groundwork/goldens/layeredsvc.branch.contract.json"},
	}
	for _, args := range verdicts {
		err := run(args)
		var v verdictError
		if !errors.As(err, &v) {
			t.Errorf("run(%v) = %v (%T), want a verdictError", args, err, err)
		}
	}
	operational := [][]string{
		{"bogus"},
		{"fitness", "/nonexistent/policy.json", "../../testdata/groundwork/goldens/layeredsvc.graph.json"},
		{"review", "p", "b"},
	}
	for _, args := range operational {
		err := run(args)
		var v verdictError
		if err == nil || errors.As(err, &v) {
			t.Errorf("run(%v) = %v (%T), want a non-verdict error", args, err, err)
		}
	}
}

// The stamp check is opt-in at both ends: silent when not asked, loud on
// mismatch or when verification was requested of an unstamped graph.
func TestVerifyStamp(t *testing.T) {
	stamped := &graph.Graph{Stamp: "abc123", Nodes: []graph.Node{}}
	bare := &graph.Graph{Nodes: []graph.Node{}}

	if err := verifyStamp(bare, "", false); err != nil {
		t.Errorf("no --expect must check nothing: %v", err)
	}
	if err := verifyStamp(stamped, "abc123", true); err != nil {
		t.Errorf("matching stamp rejected: %v", err)
	}
	if err := verifyStamp(stamped, "def456", true); err == nil {
		t.Error("mismatched stamp accepted")
	}
	if err := verifyStamp(bare, "abc123", true); err == nil {
		t.Error("unstamped graph accepted under --expect")
	}
}

// TestRunChainsOutput: the cross-service surface labels each link proven/assumed,
// prints the declared broker block flagged UNSIGNED (no warrant given), and is
// honest about the half-open chains the current fixture fleet actually has.
func TestRunChainsOutput(t *testing.T) {
	out := captureStdout(t, func() {
		if err := run([]string{"chains",
			"--service", "loansvc=../../testdata/groundwork/goldens/loansvc.graph.json",
			"--service", "obligsvc=../../testdata/groundwork/goldens/obligsvc.graph.json",
			"--policy", "../../testdata/groundwork/policies/bus-brokers.json"}); err != nil {
			t.Fatalf("chains: %v", err)
		}
	})
	for _, want := range []string{
		"chain: loan.approved",
		"[proven] producer — loansvc",
		"[proven] producer — obligsvc",
		"audit-before-publish: VIOLATED", // a real producer-side risk surfaced on the chain
		"[assumed] broker — bus",
		"UNSIGNED",                // values declared, no human warrant yet
		"open downstream",         // loan.approved has no consumer in this fleet
		"chain: payment.settled",  // consumed by loansvc
		"commits db UPDATE loans", // the consumer's downstream effect
		"open upstream",           // payment.settled has no producer in this fleet
		"dynamically-named bus effect(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("chains output missing %q:\n%s", want, out)
		}
	}
}

// TestRunChainsBrokerDedup: the bus is one thing, so a broker named identically
// by two --policy files is allowed (the same guarantee, re-stated) and still
// printed; two that DISAGREE are an error. This mirrors the mcp chains lens,
// which conflicts only on differing values.
func TestRunChainsBrokerDedup(t *testing.T) {
	loan := "../../testdata/groundwork/goldens/loansvc.graph.json"
	bus := "../../testdata/groundwork/policies/bus-brokers.json"

	// The same broker block, declared by two --policy flags → allowed, still printed.
	out := captureStdout(t, func() {
		if err := run([]string{"chains", "--service", "loansvc=" + loan,
			"--policy", bus, "--policy", bus}); err != nil {
			t.Fatalf("identical broker re-declaration must be allowed: %v", err)
		}
	})
	if !strings.Contains(out, "[assumed] broker — bus") {
		t.Errorf("the broker should still print on the card:\n%s", out)
	}

	// A second policy declaring the same broker with different values → error:
	// the bus guarantee has no single source.
	conflict := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(conflict,
		[]byte(`{"service":"other","version":1,"brokers":{"bus":{"delivery":"at-most-once"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"chains", "--service", "loansvc=" + loan,
		"--policy", bus, "--policy", conflict}); err == nil {
		t.Fatal("conflicting broker declarations across policies must be an error")
	}
}

// TestReachSurfacesBlindSpotTierAndAnnotation pins §21.C: reach surfaces the per-spot
// detail WITH the human/AI annotation context and the ExternalBoundaryCall signal/noise
// tier, so it no longer shows a thinner view than ground. The graph has one annotated,
// effect-bearing EBC.
func TestReachSurfacesBlindSpotTierAndAnnotation(t *testing.T) {
	const g = `{
  "algo":"rta",
  "nodes":[{"fqn":"pkg.Send","sig":"func()","tier":1}],
  "edges":[],
  "blind_spots":[{"kind":"ExternalBoundaryCall","site":"pkg.Send","detail":"hands off to external package acme.io/sdk; its behavior is outside the analyzed module","severity":"effect-bearing"}],
  "annotations":[{"site":"pkg.Send","kind":"ExternalBoundaryCall","note":"POSTs to acme","by":"dev@x"}]
}`
	path := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(path, []byte(g), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := run([]string{"reach", path, "pkg.Send"}); err != nil {
			t.Fatalf("reach: %v", err)
		}
	})
	if !strings.Contains(out, "blind spots on this function: 1 (1 effect-bearing external)") {
		t.Errorf("reach must show the EBC tier split in the count:\n%s", out)
	}
	if !strings.Contains(out, "[effect-bearing]") {
		t.Errorf("reach must tag the per-spot tier:\n%s", out)
	}
	if !strings.Contains(out, "🗒 POSTs to acme") || !strings.Contains(out, "dev@x") {
		t.Errorf("reach must surface the annotation note (parity with ground):\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}
