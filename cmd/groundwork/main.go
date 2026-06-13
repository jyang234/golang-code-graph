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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/chains"
	"github.com/jyang234/golang-code-graph/internal/groundwork/contract"
	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/ground"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/review"
	"github.com/jyang234/golang-code-graph/internal/groundwork/transcript"
)

var version = "dev"

// verdictError marks a computed gate verdict — a BLOCK, invariant violations, a
// breaking contract change, a tampered/stale artifact — as opposed to an
// operational failure (bad flags, unreadable inputs). The two exit differently
// so CI can tell "the change failed the gate" (exit 1) from "the gate failed to
// run" (exit 2); both stay non-zero, so an existing pass/fail gate is unchanged.
type verdictError struct{ msg string }

func (e verdictError) Error() string { return e.msg }

// verdictf builds a verdictError the way fmt.Errorf builds an error.
func verdictf(format string, args ...any) error {
	return verdictError{msg: fmt.Sprintf(format, args...)}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "groundwork:", err)
		var v verdictError
		if errors.As(err, &v) {
			os.Exit(1)
		}
		os.Exit(2)
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
	case "triage":
		return cmdTriage(args[1:])
	case "ground":
		return cmdGround(args[1:])
	case "mcp":
		return cmdMCP(args[1:])
	case "chains":
		return cmdChains(args[1:])
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
	case "exceptions":
		return cmdExceptions(args[1:])
	case "transcript":
		return cmdTranscript(args[1:])
	case "init":
		return cmdInit(args[1:])
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
  groundwork triage (--frame|--route|--table|--event|--peer) <v> [--fail] [--expect <stamp>] [--json] <graph.json>  incident triage card
  groundwork ground <graph.json> <fqn> [--policy <policy.json>] [--json]  pre-edit grounding card: what binds this function
  groundwork mcp <graph.json> [--policy <policy.json>]  serve triage/reach/ground/exceptions as MCP tools over stdio
  groundwork mcp --service <name>=<graph.json> ...      same server holding several services' maps (+ fleet-events lens)
  groundwork mcp ... --http <addr> [--token <secret>]    team-shared streamable-HTTP transport (token required off loopback)
  groundwork chains <graph.json>... [--service <name>=<graph.json>]... [--policy <p.json>]...  cross-service effect chains (CX-5, observational)
  groundwork fitness <policy.json> <graph.json> evaluate the policy's invariants (non-zero exit on violation)
  groundwork review <policy> <base.json> <branch.json> [--json]   computed MR review artifact (BLOCK exits non-zero)
  groundwork verify <policy> <base> <branch> [--scope p,q] [--json] pre-flight gate: new violations, scope creep, breaking contract
  groundwork diff <base-contract.json> <branch-contract.json>     boundary-contract diff (breaking change exits non-zero)
  groundwork verify-artifact <artifact> <policy> <base> <branch>  prove an artifact is authentic (not tampered/stale)
  groundwork exceptions <policy.json> <graph.json> [--json]      audit every allow-list entry; flag dead ones
  groundwork transcript <calls.jsonl> [--json]   summarize an mcp --log transcript: sessions, tool/service mix, cross-service hops
  groundwork init <graph.json> [--name <svc>] [--guide <out.md>]  propose a baseline policy from measured facts
  groundwork policy-check <policy.json>        load and validate a policy
  groundwork version

The graph must be produced by trusted CI (flowmap graph <service>); groundwork
only ever reads it.

exit codes: 0 clean; 1 a computed verdict failed the gate (violation, BLOCK,
breaking change, tampered/stale artifact); 2 operational error (bad flags,
unreadable inputs).
`)
}

// cmdTriage resolves an incident symptom (stack frame, DB table, bus event, or
// outbound peer) to suspect functions and prints the triage card: implicated
// entrypoints, upstream callers, reachable boundary effects, and the blind
// spots past which the card's claims are unsound. Read-only and exploratory:
// exit 0 unless the symptom resolves to nothing at all.
func cmdTriage(args []string) error {
	frame, hasFrame, args := takeValueFlag(args, "--frame", "-frame")
	route, hasRoute, args := takeValueFlag(args, "--route", "-route")
	table, hasTable, args := takeValueFlag(args, "--table", "-table")
	event, hasEvent, args := takeValueFlag(args, "--event", "-event")
	peer, hasPeer, args := takeValueFlag(args, "--peer", "-peer")
	fail, args := takeFlag(args, "--fail", "-fail")
	asJSON, args := takeFlag(args, "--json", "-json")
	expect, hasExpect, args := takeValueFlag(args, "--expect", "-expect")
	if len(args) != 1 {
		return fmt.Errorf("usage: groundwork triage (--frame|--route|--table|--event|--peer) <value> [--fail] [--expect <stamp>] [--json] <graph.json>")
	}
	set := 0
	for _, has := range []bool{hasFrame, hasRoute, hasTable, hasEvent, hasPeer} {
		if has {
			set++
		}
	}
	if set != 1 {
		// A symptom silently ignored mis-scopes an incident hunt; demand one.
		return fmt.Errorf("triage: exactly one of --frame, --route, --table, --event, --peer is required (got %d)", set)
	}
	g, err := graph.LoadFile(args[0])
	if err != nil {
		return err
	}
	if err := verifyStamp(g, expect, hasExpect); err != nil {
		return err
	}
	ix := graph.NewIndex(g)

	var res impact.Resolution
	switch {
	case hasFrame:
		res = impact.ResolveFrame(ix, frame)
	case hasRoute:
		res = impact.ResolveRoute(ix, route)
	case hasTable:
		res = impact.ResolveTable(ix, table)
	case hasEvent:
		res = impact.ResolveEvent(ix, event)
	case hasPeer:
		res = impact.ResolvePeer(ix, peer)
	}
	if len(res.Matches) == 0 && len(res.Possible) == 0 {
		return fmt.Errorf("triage: symptom resolved to nothing in this graph")
	}

	suspects := append(append([]string{}, res.Matches...), res.Possible...)
	card := impact.ForNodes(ix, suspects)
	if fail {
		card = impact.ForFault(ix, suspects)
	}
	if asJSON {
		b, err := canonjson.Marshal(struct {
			Resolution impact.Resolution `json:"resolution"`
			Card       impact.Card       `json:"card"`
		}{res, card})
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	if res.Ambiguous {
		fmt.Printf("symptom is ambiguous — %d candidates, all included:\n\n", len(res.Matches))
	}
	if len(res.Possible) > 0 {
		fmt.Printf("⚠️  %d possible match(es) via <dynamic> boundary effects, included and flagged\n\n", len(res.Possible))
	}
	fmt.Print(card.Render())
	return nil
}

// verifyStamp enforces an opt-in identity check: when the caller says which
// code they believe the graph describes (--expect, typically the deployed
// SHA), a missing or mismatched stamp fails loudly — a stale map mis-triages,
// and silently so. When not asked, nothing is checked and nothing is printed:
// a routine local run against a freshly generated graph must not cry wolf.
func verifyStamp(g *graph.Graph, expect string, hasExpect bool) error {
	if !hasExpect {
		return nil
	}
	if g.Stamp == "" {
		return fmt.Errorf("graph carries no stamp but --expect %q was given; regenerate with `flowmap graph --stamp <sha>` in CI", expect)
	}
	if g.Stamp != expect {
		return fmt.Errorf("graph stamp %q does not match --expect %q — this is not the graph for the code you think it is", g.Stamp, expect)
	}
	return nil
}

// cmdGround prints the pre-edit grounding card for one function: identity,
// neighborhood, reachable effects, the rules that demonstrably bind it, and
// the blind spots touching those claims. The same rules that gate the merge,
// surfaced BEFORE the edit — ground → edit → verify, one rule set at both
// ends. The policy is optional; without it the card carries the graph-borne
// facts only.
func cmdGround(args []string) error {
	policyPath, hasPolicy, args := takeValueFlag(args, "--policy", "-policy")
	asJSON, args := takeFlag(args, "--json", "-json")
	if len(args) != 2 {
		return fmt.Errorf("usage: groundwork ground <graph.json> <fqn> [--policy <policy.json>] [--json]")
	}
	g, err := graph.LoadFile(args[0])
	if err != nil {
		return err
	}
	var p *policy.Policy
	if hasPolicy {
		if p, err = policy.Load(policyPath); err != nil {
			return err
		}
	}
	card, err := ground.For(graph.NewIndex(g), p, args[1])
	if err != nil {
		return err
	}
	if asJSON {
		b, err := canonjson.Marshal(card)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(card.Render())
	return nil
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

// cmdChains composes the cross-service effect-chain cards (CX-5). It loads a
// fleet of service graphs, joins their publishers to consumers by event name,
// and prints — per event — a happens-before chain whose links are labeled
// proven (a per-service graph fact: a publish's commit ordering, a consumer
// handler's effects) or assumed (the declared broker guarantee, never
// inferred). The broker block, if any, is read from the --policy file(s).
//
// It is observational and always exits 0: it surfaces what already committed
// and what a consumer does with an event, it never gates. A chain that has no
// producer or no consumer in the loaded fleet is printed as open, not hidden.
func cmdChains(args []string) error {
	servicePairs, args := takeValueFlags(args, "--service", "-service")
	policyPaths, args := takeValueFlags(args, "--policy", "-policy")

	type svcSpec struct{ name, path string }
	var specs []svcSpec
	seen := map[string]bool{}
	addSpec := func(name, path string) error {
		if !validServiceName(name) {
			return fmt.Errorf("service name %q: letters, digits, '.', '_', '-' only", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate service name %q", name)
		}
		seen[name] = true
		specs = append(specs, svcSpec{name: name, path: path})
		return nil
	}
	for _, pair := range servicePairs {
		name, path, ok := strings.Cut(pair, "=")
		if !ok || name == "" || path == "" {
			return fmt.Errorf("--service wants <name>=<graph.json>, got %q", pair)
		}
		if err := addSpec(name, path); err != nil {
			return err
		}
	}
	for _, path := range args {
		if err := addSpec(serviceNameFromPath(path), path); err != nil {
			return err
		}
	}
	if len(specs) == 0 {
		return fmt.Errorf("groundwork chains needs at least one graph: pass <graph.json> positionally or with --service name=graph.json")
	}

	var fleet []chains.Service
	for _, s := range specs {
		g, err := graph.LoadFile(s.path)
		if err != nil {
			return err
		}
		fleet = append(fleet, chains.Service{Name: s.name, Index: graph.NewIndex(g)})
	}

	// The broker guarantee is fleet-wide: the bus is one thing, so it must have
	// one declared source. Reading it from several policies that disagree would
	// print a guarantee no one authored — refuse rather than pick.
	brokers := map[string]policy.Broker{}
	for _, pp := range policyPaths {
		path := pp
		if _, p, ok := strings.Cut(pp, "="); ok {
			path = p
		}
		pol, err := policy.Load(path)
		if err != nil {
			return err
		}
		for name, b := range pol.Brokers {
			if _, dup := brokers[name]; dup {
				return fmt.Errorf("broker %q declared by more than one --policy; the bus guarantee must have a single source", name)
			}
			brokers[name] = b
		}
	}

	fmt.Print(chains.Build(fleet, brokers).Render())
	return nil
}

// serviceNameFromPath derives a service name from a graph file path for the
// positional form, stripping the conventional ".graph.json" (or ".json") suffix.
func serviceNameFromPath(path string) string {
	base := filepath.Base(path)
	for _, suf := range []string{".graph.json", ".json"} {
		if strings.HasSuffix(base, suf) {
			return strings.TrimSuffix(base, suf)
		}
	}
	return base
}

// cmdFitness evaluates a policy's invariants against a graph. It prints every
// finding — violations (which fail the gate) and cautions (the graph abstaining
// where it cannot prove a negative) — and returns an error so CI exits non-zero
// when any invariant is broken.
func cmdFitness(args []string) error {
	sarif, args := takeFlag(args, "--sarif", "-sarif")
	fs := flag.NewFlagSet("fitness", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: groundwork fitness <policy.json> <graph.json> [--sarif]")
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

	if sarif {
		b, err := toSARIF(res.Findings)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		if !res.OK() {
			return verdictf("%d invariant violation(s)", len(res.Violations()))
		}
		return nil
	}
	violations, cautions := res.Violations(), res.Cautions()
	for _, f := range violations {
		printFinding("⛔", f)
	}
	for _, f := range cautions {
		printFinding("⚠️ ", f)
	}
	if !res.OK() {
		return verdictf("%d invariant violation(s)", len(violations))
	}
	// The summary reports what Check actually evaluated: policy rules PLUS the
	// graph-carried obligation verdicts. "0 invariant(s)" while obligations
	// were judged would misreport the gate's coverage.
	summary := fmt.Sprintf("fitness OK — %d invariant(s) hold", ruleCount(p))
	if n := len(g.Obligations); n > 0 {
		summary += fmt.Sprintf(", %d obligation verdict(s) judged", n)
	}
	fmt.Printf("%s, %d caution(s)\n", summary, len(cautions))
	return nil
}

// edgeLine renders a finding's exact edge or symbol.
func edgeLine(f fitness.Finding) string {
	if f.To != "" {
		return f.From + " → " + f.To
	}
	return f.From
}

// printFinding renders one finding, Detail included — a caution's witness is
// as load-bearing as a violation's.
func printFinding(prefix string, f fitness.Finding) {
	fmt.Printf("%s [%s] %s\n", prefix, f.Rule, f.Summary)
	if f.From != "" {
		fmt.Printf("     %s\n", edgeLine(f))
	}
	if f.Detail != "" {
		fmt.Printf("     via %s\n", f.Detail)
	}
}

// ruleCount is a rough tally of configured invariants, for the OK summary.
func ruleCount(p *policy.Policy) int {
	n := len(p.MustNotReach) + len(p.MustPassThrough) + len(p.NoConcurrentReach)
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
		return verdictf("review verdict: BLOCK")
	}
	return nil
}

// cmdVerify runs the pre-flight gate over a base/branch graph pair: it blocks on
// any newly-introduced violation, on a touched package outside the declared
// --scope, or on a breaking contract change. Exits non-zero on BLOCK.
func cmdVerify(args []string) error {
	scopeArg, _, rest := takeValueFlag(args, "--scope", "-scope")
	asJSON, rest := takeFlag(rest, "--json", "-json")
	if len(rest) != 3 {
		return fmt.Errorf("usage: groundwork verify <policy.json> <base-graph.json> <branch-graph.json> [--scope pkg,pkg] [--json]")
	}
	scope := splitComma(scopeArg)
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
		return verdictf("verify: BLOCK")
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
		return verdictf("diff: breaking contract change(s)")
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
		return verdictf("artifact is %s", res.Status)
	}
	return nil
}

// takeValueFlags removes every occurrence of a value flag ("--name v" or
// "--name=v") from args in any position, returning the values in order of
// appearance. One scanning mechanism for every subcommand — stdlib
// flag.Parse stops at the first positional, so each hand-rolled parser is a
// usage string waiting to disagree with reality.
func takeValueFlags(args []string, names ...string) (values []string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		matched := false
		for _, n := range names {
			if a == n && i+1 < len(args) {
				values, matched = append(values, args[i+1]), true
				i++
				break
			}
			if strings.HasPrefix(a, n+"=") {
				values, matched = append(values, strings.TrimPrefix(a, n+"=")), true
				break
			}
		}
		if !matched {
			rest = append(rest, a)
		}
	}
	return values, rest
}

// takeValueFlag is the singular form: every occurrence is removed, the last
// value wins. A thin wrapper so the scanning loop exists exactly once.
func takeValueFlag(args []string, names ...string) (value string, found bool, rest []string) {
	values, rest := takeValueFlags(args, names...)
	if len(values) == 0 {
		return "", false, rest
	}
	return values[len(values)-1], true, rest
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

// cmdExceptions audits the policy's allow-lists against a graph: every active
// suppression is listed with its reason, and entries that no longer suppress
// anything are flagged DEAD — stale excuses that should be deleted before they
// silently excuse something new. Read-only, exit 0: it informs review.
func cmdExceptions(args []string) error {
	asJSON, rest := takeFlag(args, "--json", "-json")
	if len(rest) != 2 {
		return fmt.Errorf("usage: groundwork exceptions <policy.json> <graph.json> [--json]")
	}
	p, err := policy.Load(rest[0])
	if err != nil {
		return err
	}
	g, err := graph.LoadFile(rest[1])
	if err != nil {
		return err
	}
	ix := graph.NewIndex(g)
	xs := fitness.Exceptions(p, ix)
	if xs == nil {
		xs = []fitness.ExceptionStatus{} // canonical [] rather than null
	}
	ls := fitness.Liveness(p, ix)
	if ls == nil {
		ls = []fitness.PatternLiveness{}
	}
	if asJSON {
		b, err := canonjson.Marshal(struct {
			Exceptions   []fitness.ExceptionStatus `json:"exceptions"`
			RuleLiveness []fitness.PatternLiveness `json:"rule_liveness"`
		}{xs, ls})
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	if len(xs) == 0 && len(ls) == 0 {
		fmt.Println("no allow-list entries or pattern-bearing rules configured")
		return nil
	}
	for _, x := range xs {
		fmt.Println(x)
	}
	for _, l := range ls {
		fmt.Println(l)
	}
	if dead := fitness.DeadCount(xs); dead > 0 {
		fmt.Printf("\n%d dead exception(s) — delete them: a stale excuse can silently cover a future violation\n", dead)
	} else if len(xs) > 0 {
		fmt.Printf("\nall %d exception(s) live and justified\n", len(xs))
	}
	if dead := fitness.DeadPatternCount(ls); dead > 0 {
		fmt.Printf("%d dead rule pattern(s) — fix or delete them: a rule that binds nothing guards nothing\n", dead)
	}
	return nil
}

// cmdTranscript summarizes an `mcp --log` transcript: the reader half of the
// E4 measurement apparatus, and the evidence the MCP tiers 2–3 plan defers
// to. Counts only — per-session query volume, tool and service mix,
// cross-service hops, error/correction rates; the qualitative half of E4
// (do conclusions cite card facts?) stays human-judged and the card says so.
// Read-only, exit 0: it informs a keep/retire decision, it does not make one.
func cmdTranscript(args []string) error {
	asJSON, rest := takeFlag(args, "--json", "-json")
	if len(rest) != 1 {
		return fmt.Errorf("usage: groundwork transcript <calls.jsonl> [--json]")
	}
	entries, err := transcript.Load(rest[0])
	if err != nil {
		return err
	}
	s := transcript.Summarize(entries)
	if asJSON {
		b, err := canonjson.Marshal(s)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(transcript.Render(s))
	return nil
}

// cmdInit proposes a baseline policy derived from the graph's measured facts
// (the cold-start answer): the policy JSON goes to stdout (or --out), the
// review guide — evidence, tightening instructions, latent findings, and the
// questions only the team can answer — to --guide. Everything emitted is a
// ratchet of current truth, self-verified clean against the source graph; a
// CODEOWNER reviews and commits.
func cmdInit(args []string) error {
	name, hasName, args := takeValueFlag(args, "--name", "-name")
	guidePath, hasGuide, args := takeValueFlag(args, "--guide", "-guide")
	outPath, hasOut, args := takeValueFlag(args, "--out", "-out")
	if len(args) != 1 {
		return fmt.Errorf("usage: groundwork init <graph.json> [--name <service>] [--out <policy.json>] [--guide <guide.md>]")
	}
	g, err := graph.LoadFile(args[0])
	if err != nil {
		return err
	}
	ix := graph.NewIndex(g)
	if !hasName {
		name = inferServiceName(ix)
	}
	p, guideMD := fitness.Propose(ix, name)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("init produced an invalid policy (bug): %w", err)
	}
	b, err := canonjson.Marshal(p)
	if err != nil {
		return err
	}
	if hasOut {
		if err := os.WriteFile(outPath, append(b, 10), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "proposed policy written to %s\n", outPath)
	} else {
		fmt.Println(string(b))
	}
	if hasGuide {
		if err := os.WriteFile(guidePath, []byte(guideMD), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "review guide written to %s — it is the refining agent's checklist\n", guidePath)
	} else {
		fmt.Fprintf(os.Stderr, "tip: --guide <file.md> writes the review guide (evidence, tightening steps, latent findings)\n")
	}
	return nil
}

// inferServiceName takes the last segment of the longest common package
// prefix of the graph's nodes — the module root's name, in practice.
func inferServiceName(ix *graph.Index) string {
	nodes := ix.Nodes()
	if len(nodes) == 0 {
		return "service"
	}
	prefix := fitness.PkgOf(nodes[0])
	for _, fqn := range nodes[1:] {
		p := fitness.PkgOf(fqn)
		for !strings.HasPrefix(p, prefix) && prefix != "" {
			if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
				prefix = prefix[:i]
			} else {
				prefix = ""
			}
		}
	}
	if i := strings.LastIndexByte(prefix, '/'); i >= 0 {
		prefix = prefix[i+1:]
	}
	if prefix == "" {
		return "service"
	}
	return prefix
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
	if n := len(p.MustPassThrough); n > 0 {
		fmt.Printf("  must_pass_through: %d rule(s)\n", n)
	}
	if n := len(p.NoConcurrentReach); n > 0 {
		fmt.Printf("  no_concurrent_reach: %d rule(s)\n", n)
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
	if r := p.EffectRatchet; r != nil {
		mode := "observe-only"
		if r.Gate {
			mode = "gating"
		}
		fmt.Printf("  effect_ratchet: %s, %d allow-listed target(s)\n", mode, len(r.Allow))
	}
	return nil
}
