// Command groundwork is the deterministic verification layer over flowmap's call
// graph. It consumes the graph JSON flowmap emits (never generating it itself —
// the graph must come from trusted CI) and computes architectural fitness,
// blast-radius, pre-merge gates and review artifacts. No AI sits in any verdict;
// every output is a pure function of (graph, policy, delta).
//
// Phase 0 ships the substrate and two introspection surfaces — `reach` (graph
// reachability) and `policy-check` (validate a policy) — that the later
// verdict-bearing surfaces (fitness, verify, review) build on.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/contract"
	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/review"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "groundwork:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "version":
		fmt.Println("groundwork", version)
		return nil
	case "reach":
		return cmdReach(args[1:])
	case "fitness":
		return cmdFitness(args[1:])
	case "review":
		return cmdReview(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "diff":
		return cmdDiff(args[1:])
	case "verify-artifact":
		return cmdVerifyArtifact(args[1:])
	case "policy-check":
		return cmdPolicyCheck(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (try `groundwork help`)", args[0])
	}
}

func usage() {
	fmt.Print(`groundwork — deterministic verification over flowmap's call graph

usage:
  groundwork reach <graph.json> <fqn>          reachability + entrypoint cover + effects for a function
  groundwork fitness <policy.json> <graph.json> evaluate the policy's invariants (non-zero exit on violation)
  groundwork review <policy> <base.json> <branch.json> [--json]   computed MR review artifact (BLOCK exits non-zero)
  groundwork verify <policy> <base> <branch> [--scope p,q] [--json] pre-flight gate: new violations, scope creep, breaking contract
  groundwork diff <base-contract.json> <branch-contract.json>     boundary-contract diff (breaking change exits non-zero)
  groundwork verify-artifact <artifact> <policy> <base> <branch>  prove an artifact is authentic (not tampered/stale)
  groundwork policy-check <policy.json>        load and validate a policy
  groundwork version

The graph must be produced by trusted CI (flowmap graph <service>); groundwork
only ever reads it.
`)
}

// cmdReach reports the bidirectional reachability of one function: who breaks if
// it changes (callers), what it depends on (callees), which entry points it is
// live behind, the external effects it reaches, and any blind spots on it.
func cmdReach(args []string) error {
	fs := flag.NewFlagSet("reach", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: groundwork reach <graph.json> <fqn>")
	}
	g, err := graph.LoadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	ix := graph.NewIndex(g)
	fqn := fs.Arg(1)
	if !ix.Has(fqn) {
		return fmt.Errorf("no function %q in graph (it has %d nodes)", fqn, len(g.Nodes))
	}

	callers := ix.Reaching(fqn)
	callees := ix.Reachable(fqn)
	cover := ix.EntrypointCover(fqn)
	effects := ix.Effects(append([]string{fqn}, callees...)...)

	fmt.Printf("%s\n\n", fqn)
	fmt.Printf("transitive callers (blast radius): %d\n", len(callers))
	for _, c := range callers {
		fmt.Printf("  ← %s\n", c)
	}
	fmt.Printf("transitive callees (dependencies): %d\n", len(callees))
	for _, c := range callees {
		fmt.Printf("  → %s\n", c)
	}
	fmt.Printf("live behind %d entrypoint(s):\n", len(cover))
	for _, e := range cover {
		fmt.Printf("  ⮕ %s\n", e)
	}
	fmt.Printf("reachable external effects: %d\n", len(effects))
	for _, e := range effects {
		marker := ""
		if e.IsDynamic() {
			marker = "  ⚠ unresolved (soundness frontier)"
		}
		fmt.Printf("  %s [%s]%s\n", strings.TrimPrefix(e.To, "boundary:"), e.Boundary, marker)
	}
	if bs := ix.BlindSpotsAt(fqn); len(bs) > 0 {
		fmt.Printf("blind spots on this function: %d\n", len(bs))
		for _, b := range bs {
			fmt.Printf("  ⚠ %s — %s\n", b.Kind, b.Detail)
		}
	}
	return nil
}

// cmdFitness evaluates a policy's invariants against a graph. It prints every
// finding — violations (which fail the gate) and cautions (the graph abstaining
// where it cannot prove a negative) — and returns an error so CI exits non-zero
// when any invariant is broken.
func cmdFitness(args []string) error {
	fs := flag.NewFlagSet("fitness", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: groundwork fitness <policy.json> <graph.json>")
	}
	p, err := policy.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	g, err := graph.LoadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	res := fitness.Check(p, graph.NewIndex(g))

	violations, cautions := res.Violations(), res.Cautions()
	for _, f := range violations {
		fmt.Printf("⛔ [%s] %s\n", f.Rule, f.Summary)
		if f.From != "" {
			fmt.Printf("     %s\n", edgeLine(f))
		}
	}
	for _, f := range cautions {
		fmt.Printf("⚠️  [%s] %s\n", f.Rule, f.Summary)
		if f.From != "" {
			fmt.Printf("     %s\n", edgeLine(f))
		}
	}
	if !res.OK() {
		return fmt.Errorf("%d invariant violation(s)", len(violations))
	}
	fmt.Printf("fitness OK — %d invariant(s) hold, %d caution(s)\n", ruleCount(p), len(cautions))
	return nil
}

// edgeLine renders a finding's exact edge or symbol.
func edgeLine(f fitness.Finding) string {
	if f.To != "" {
		return f.From + " → " + f.To
	}
	return f.From
}

// ruleCount is a rough tally of configured invariants, for the OK summary.
func ruleCount(p *policy.Policy) int {
	n := len(p.MustNotReach)
	if p.Layering != nil {
		n++
	}
	if p.IOBudget != nil {
		n++
	}
	return n
}

// cmdReview computes the base-vs-branch MR review artifact. With --json it emits
// the canonical artifact (the form a verifier reads); otherwise the human report.
// A BLOCK verdict exits non-zero so the same command can back a CI gate.
func cmdReview(args []string) error {
	asJSON, rest := takeFlag(args, "--json", "-json")
	if len(rest) != 3 {
		return fmt.Errorf("usage: groundwork review <policy.json> <base-graph.json> <branch-graph.json> [--json]")
	}
	p, base, branch, err := loadReviewInputs(rest[0], rest[1], rest[2])
	if err != nil {
		return err
	}
	art := review.Review(p, base, branch)

	if asJSON {
		b, err := art.Marshal()
		if err != nil {
			return err
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return err
		}
	} else {
		fmt.Print(art.Render())
	}
	if art.Verdict == review.Block {
		return fmt.Errorf("review verdict: BLOCK")
	}
	return nil
}

// cmdVerify runs the pre-flight gate over a base/branch graph pair: it blocks on
// any newly-introduced violation, on a touched package outside the declared
// --scope, or on a breaking contract change. Exits non-zero on BLOCK.
func cmdVerify(args []string) error {
	var rest, scope []string
	asJSON := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			asJSON = true
		case a == "--scope":
			if i++; i < len(args) {
				scope = splitComma(args[i])
			}
		case strings.HasPrefix(a, "--scope="):
			scope = splitComma(strings.TrimPrefix(a, "--scope="))
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 3 {
		return fmt.Errorf("usage: groundwork verify <policy.json> <base-graph.json> <branch-graph.json> [--scope pkg,pkg] [--json]")
	}
	p, base, branch, err := loadReviewInputs(rest[0], rest[1], rest[2])
	if err != nil {
		return err
	}
	g := review.Gate(p, base, branch, scope)

	if asJSON {
		b, err := g.Marshal()
		if err != nil {
			return err
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return err
		}
	} else {
		fmt.Print(g.Render())
	}
	if !g.Pass {
		return fmt.Errorf("verify: BLOCK")
	}
	return nil
}

// cmdDiff compares two boundary contracts and reports the inter-service surface
// movement. Exits non-zero when a change is breaking.
func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: groundwork diff <base-contract.json> <branch-contract.json>")
	}
	base, err := contract.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	branch, err := contract.Load(fs.Arg(1))
	if err != nil {
		return err
	}
	d := contract.Compare(base, branch)
	if d.Empty() {
		fmt.Println("no boundary-contract changes")
		return nil
	}
	for _, c := range d.Changes {
		tag := ""
		if c.Breaking {
			tag = "  ⚠ BREAKING"
		}
		fmt.Printf("%s %s %s%s\n", c.Op, c.Surface, c.Name, tag)
	}
	if d.Breaking() {
		return fmt.Errorf("diff: breaking contract change(s)")
	}
	return nil
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cmdVerifyArtifact recomputes an artifact from the source graphs and reports
// whether it is authentic, tampered, or stale. The graphs must be CI-trusted.
func cmdVerifyArtifact(args []string) error {
	fs := flag.NewFlagSet("verify-artifact", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 4 {
		return fmt.Errorf("usage: groundwork verify-artifact <artifact.json> <policy.json> <base-graph.json> <branch-graph.json>")
	}
	art, err := review.LoadArtifact(fs.Arg(0))
	if err != nil {
		return err
	}
	p, base, branch, err := loadReviewInputs(fs.Arg(1), fs.Arg(2), fs.Arg(3))
	if err != nil {
		return err
	}
	res := review.VerifyArtifact(art, p, base, branch)
	fmt.Printf("%s — %s\n", res.Status, res.Detail)
	if !res.OK() {
		return fmt.Errorf("artifact is %s", res.Status)
	}
	return nil
}

// takeFlag removes any of the given boolean flag spellings from args, reporting
// whether one was present. It lets a flag appear anywhere, including after the
// positional arguments (Go's flag package stops at the first positional).
func takeFlag(args []string, names ...string) (found bool, rest []string) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, a := range args {
		if want[a] {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

// loadReviewInputs loads the policy and the two graphs the review surfaces share.
func loadReviewInputs(policyPath, basePath, branchPath string) (*policy.Policy, *graph.Graph, *graph.Graph, error) {
	p, err := policy.Load(policyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	base, err := graph.LoadFile(basePath)
	if err != nil {
		return nil, nil, nil, err
	}
	branch, err := graph.LoadFile(branchPath)
	if err != nil {
		return nil, nil, nil, err
	}
	return p, base, branch, nil
}

// cmdPolicyCheck loads and validates a policy, printing a one-line-per-rule
// summary. It is the lint surface for the CODEOWNERS-gated policy file.
func cmdPolicyCheck(args []string) error {
	fs := flag.NewFlagSet("policy-check", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: groundwork policy-check <policy.json>")
	}
	p, err := policy.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("policy for %q (v%d) — valid\n", p.Service, p.Version)
	if len(p.Layers) > 0 {
		fmt.Printf("  layers (top→bottom): %s\n", strings.Join(p.LayerNames(), " → "))
	}
	if p.Layering != nil {
		fmt.Printf("  layering: %d allow-listed exception(s), %d root package(s)\n", len(p.Layering.Allow), len(p.Layering.Roots))
	}
	if n := len(p.MustNotReach); n > 0 {
		fmt.Printf("  must_not_reach: %d rule(s)\n", n)
	}
	if p.IOBudget != nil {
		fmt.Printf("  io_budget: max %d write(s) per route\n", p.IOBudget.MaxWritesPerRoute)
	}
	if r := p.BlindSpotRatchet; r != nil {
		mode := "observe-only"
		if r.Gate {
			mode = "gating"
		}
		fmt.Printf("  blind_spot_ratchet: %s, %d allow-listed exception(s)\n", mode, len(r.Allow))
	}
	return nil
}
