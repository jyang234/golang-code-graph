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

// TestUsageBodyDocumentsEverySubcommand pins M-33's help/flag parity: every
// subcommand the dispatcher accepts must have a line in usageBody (which
// printSubUsage scans for per-command help), and every gate command's disclosed
// flags must be documented. A subcommand present in the switch but absent from
// usageBody would answer `-h` with the whole-usage fallback, hiding its own shape.
func TestUsageBodyDocumentsEverySubcommand(t *testing.T) {
	subcommands := []string{
		"reach", "triage", "ground", "mcp", "chains", "fitness", "review",
		"review-triage", "verify", "diff", "verify-artifact", "exceptions",
		"transcript", "init", "policy-check", "version",
	}
	for _, cmd := range subcommands {
		if !strings.Contains(usageBody, "groundwork "+cmd+" ") && !strings.Contains(usageBody, "groundwork "+cmd+"\n") {
			t.Errorf("subcommand %q has no usageBody line — `groundwork %s -h` would fall back to the whole usage", cmd, cmd)
		}
	}
	// The flags the audit found missing must stay documented (they were silently
	// omitted, so their help never mentioned them).
	mustDoc := map[string]string{
		"fitness --sarif":  "--sarif",
		"verify --corpus":  "--corpus",
		"verify --capture": "--capture",
		"init --out":       "--out",
	}
	for what, flag := range mustDoc {
		if !strings.Contains(usageBody, flag) {
			t.Errorf("usageBody omits %s (%s) — the sub-help would not list a real flag", flag, what)
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

// TestReachDisclosesForwardHighFanOut pins the A2 disclosure: when the FORWARD reach
// crosses a HighFanOut dispatch seam (the context-insensitive graph fans one dispatch
// site onto every closure that flows to it), the CLI's effect count is an upper bound
// that may include sibling-closure effects — so reach must say so, mirroring the
// cover-side disclosure and the MCP lens. A clean query whose forward cone never
// crosses such a seam must stay silent (no false caveat).
func TestReachDisclosesForwardHighFanOut(t *testing.T) {
	// Handle → RunInTx (a HighFanOut seam) → two sibling closures, each writing. From
	// Handle the forward reach fans onto BOTH closures' writes (the union over-report).
	// Read → readRow is a clean forward cone with one effect and no dispatch seam.
	const g = `{
  "algo":"rta",
  "nodes":[
    {"fqn":"pkg.Handle","sig":"func()","tier":1},
    {"fqn":"pkg.RunInTx","sig":"func()","tier":1},
    {"fqn":"pkg.closeA","sig":"func()","tier":1},
    {"fqn":"pkg.closeB","sig":"func()","tier":1},
    {"fqn":"pkg.Read","sig":"func()","tier":1},
    {"fqn":"pkg.readRow","sig":"func()","tier":1}
  ],
  "edges":[
    {"from":"pkg.Handle","to":"pkg.RunInTx","tier":1},
    {"from":"pkg.RunInTx","to":"pkg.closeA","tier":1},
    {"from":"pkg.RunInTx","to":"pkg.closeB","tier":1},
    {"from":"pkg.closeA","to":"boundary:db INSERT a","tier":1,"boundary":"outbound-sync"},
    {"from":"pkg.closeB","to":"boundary:db INSERT b","tier":1,"boundary":"outbound-sync"},
    {"from":"pkg.Read","to":"pkg.readRow","tier":1},
    {"from":"pkg.readRow","to":"boundary:db SELECT r","tier":1,"boundary":"outbound-sync"}
  ],
  "blind_spots":[{"kind":"HighFanOut","site":"pkg.RunInTx","detail":"dispatch resolved to many callees"}]
}`
	path := filepath.Join(t.TempDir(), "g.json")
	if err := os.WriteFile(path, []byte(g), 0o644); err != nil {
		t.Fatal(err)
	}

	// The over-reporter: forward reach crosses the HighFanOut, so the effect count is
	// disclosed as an upper bound that may include sibling-closure effects.
	over := captureStdout(t, func() {
		if err := run([]string{"reach", path, "pkg.Handle"}); err != nil {
			t.Fatalf("reach Handle: %v", err)
		}
	})
	if !strings.Contains(over, "reachable external effects: 2 ≤ (over-approx via dispatch") {
		t.Errorf("reach must disclose the forward HighFanOut over-approx on effects:\n%s", over)
	}
	if !strings.Contains(over, "sibling-closure effects past a HighFanOut seam") {
		t.Errorf("reach must name the sibling-closure cause:\n%s", over)
	}

	// The clean read query: forward cone never crosses a dispatch seam, so the effect
	// count must carry NO caveat (no false disclosure).
	clean := captureStdout(t, func() {
		if err := run([]string{"reach", path, "pkg.Read"}); err != nil {
			t.Fatalf("reach Read: %v", err)
		}
	})
	if !strings.Contains(clean, "reachable external effects: 1\n") {
		t.Errorf("clean read query effect count should be bare (no caveat):\n%s", clean)
	}
	if strings.Contains(clean, "over-approx via dispatch") {
		t.Errorf("clean read query must not emit a HighFanOut caveat:\n%s", clean)
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
