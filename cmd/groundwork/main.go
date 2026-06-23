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
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/internal/buildinfo"
	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/chains"
	"github.com/jyang234/golang-code-graph/internal/groundwork/contract"
	"github.com/jyang234/golang-code-graph/internal/groundwork/fitness"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/groundwork/ground"
	"github.com/jyang234/golang-code-graph/internal/groundwork/impact"
	"github.com/jyang234/golang-code-graph/internal/groundwork/policy"
	"github.com/jyang234/golang-code-graph/internal/groundwork/review"
	"github.com/jyang234/golang-code-graph/internal/groundwork/reviewtriage"
	"github.com/jyang234/golang-code-graph/internal/groundwork/transcript"
	"github.com/jyang234/golang-code-graph/internal/impeach"
	"github.com/jyang234/golang-code-graph/ir"
)

// version is overridden at build time via -ldflags "-X main.version=...". When
// unset, buildinfo.Version recovers the module/VCS stamp Go embeds so an
// installed binary still names itself (see internal/buildinfo).
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
	// Consistent help across subcommands: `groundwork <cmd> -h/--help` prints that
	// command's own usage and exits 0. Intercepting here — before each subcommand's
	// own parsing — fixes the three divergent behaviors (a FlagSet's empty "Usage of
	// fitness:", and the positional-first init reading "-h" as <graph.json>) in one
	// place. The bare top-level help forms are handled by the switch below.
	if cmd := args[0]; cmd != "help" && cmd != "-h" && cmd != "--help" && helpRequested(args[1:]) {
		printSubUsage(cmd)
		return nil
	}
	switch args[0] {
	case "version":
		fmt.Println("groundwork", buildinfo.Version(version))
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
	case "review-triage":
		return cmdReviewTriage(args[1:])
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

func usage() { fmt.Print(usageBody) }

// helpRequested reports whether any arg is the help flag. It checks only -h/--help
// (not a bare "help" token) so a positional value never trips it.
func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// printSubUsage prints the usage line(s) for one subcommand, scanned from the
// master usageBody (mcp has several). It falls back to the full usage when a
// command has no dedicated line, so help is never empty.
func printSubUsage(cmd string) {
	var lines []string
	for _, line := range strings.Split(usageBody, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "groundwork "+cmd+" ") || t == "groundwork "+cmd {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		usage()
		return
	}
	fmt.Println("usage:")
	for _, l := range lines {
		fmt.Println("  " + l)
	}
}

// usageBody is the master usage text. Per-subcommand help (printSubUsage) scans it
// so `groundwork <cmd> -h` answers with that command's own line — the single
// source so the two views cannot drift.
const usageBody = `groundwork — deterministic verification over flowmap's call graph

usage:
  groundwork reach <graph.json> <fqn>          reachability + entrypoint cover + effects for a function
  groundwork triage (--frame|--route|--table|--event|--peer) <v> [--fail] [--expect <stamp>] [--json] <graph.json>  incident triage card
  groundwork ground <graph.json> <fqn> [--policy <policy.json>] [--json]  pre-edit grounding card: what binds this function
  groundwork mcp <graph.json> [--policy <policy.json>]  serve triage/reach/ground/exceptions as MCP tools over stdio
  groundwork mcp --service <name>=<graph.json> ...      same server holding several services' maps (+ fleet-events lens)
  groundwork mcp ... --http <addr> [--token <secret>]    team-shared streamable-HTTP transport (token required off loopback)
  groundwork chains <graph.json>... [--service <name>=<graph.json>]... [--policy <p.json>]...  cross-service effect chains (CX-5, observational)
  groundwork fitness <policy.json> <graph.json> [--expect <sha>] evaluate the policy's invariants (non-zero exit on violation)
  groundwork review <policy> <base.json> <branch.json> [--expect <sha>] [--json]   computed MR review artifact (BLOCK exits non-zero)
  groundwork review-triage <base.json> <branch.json> [--json|--mermaid|--summary] [--policy <p.json>] [--full] [--max-nodes N]   PROTOTYPE: 3-zone reviewer triage; --summary is an MR-comment digest; --policy adds per-route write movement
  groundwork verify <policy> <base> <branch> [--scope p,q] [--expect <sha>] [--json] pre-flight gate: new violations, scope creep, breaking contract
  groundwork diff <base-contract.json> <branch-contract.json>     boundary-contract diff (breaking change exits non-zero)
  groundwork verify-artifact <artifact> <policy> <base> <branch> [--expect <sha>]  prove an artifact is authentic (not tampered/stale)

The gate commands (fitness/review/verify/verify-artifact) take --expect <sha> to
bind the verdict to the code under review: it must equal the stamp the graph was
produced with (flowmap graph --stamp <sha>), so a stale graph can't gate the
wrong code. Set GROUNDWORK_REQUIRE_STAMP=1 in CI to make --expect mandatory.
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
`

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

// requireStampEnv, when set truthy, makes the identity check MANDATORY on the
// verdict-bearing gate commands. In CI you set it so a forgotten --expect (or an
// unstamped graph) fails the gate loudly instead of silently gating whatever
// graph the command was handed — the difference between "the check exists" and
// "the check can't be skipped".
const requireStampEnv = "GROUNDWORK_REQUIRE_STAMP"

func stampRequired() bool {
	switch strings.ToLower(os.Getenv(requireStampEnv)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// verifyGateStamp is verifyStamp for the verdict-bearing gate commands
// (fitness/review/verify/verify-artifact). It is identical to verifyStamp —
// opt-in, silent when not asked — except that when requireStampEnv is set, an
// absent --expect is itself an error. That closes the gap where a gate could run
// (and PASS) against a graph whose identity to the code under review was never
// checked: a stale-but-schema-valid graph would otherwise produce a confident,
// wrong green. For a two-graph command the stamp binds the BRANCH graph — the
// code being gated — not the historical base.
func verifyGateStamp(g *graph.Graph, expect string, hasExpect bool) error {
	if !hasExpect && stampRequired() {
		return fmt.Errorf("%s is set but no --expect was given: a gate command must bind its verdict to the code under review — pass --expect <sha>, the same value used for `flowmap graph --stamp <sha>`", requireStampEnv)
	}
	return verifyStamp(g, expect, hasExpect)
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
	forward := append([]string{fqn}, callees...)
	effects := ix.Effects(forward...)

	fmt.Printf("%s\n\n", fqn)
	fmt.Printf("transitive callers (blast radius): %d\n", len(callers))
	for _, c := range callers {
		fmt.Printf("  ← %s\n", c)
	}
	fmt.Printf("transitive callees (dependencies): %d\n", len(callees))
	for _, c := range callees {
		fmt.Printf("  → %s\n", c)
	}
	coverNote := ""
	if ix.CrossesHighFanOut(callers) {
		// The reverse reach crossed a HighFanOut dispatch seam — the cover fans
		// every caller onto every implementation, so this count is an upper bound.
		coverNote = graph.OverApproxCoverNote
	}
	fmt.Printf("live behind %d entrypoint(s)%s:\n", len(cover), coverNote)
	for _, e := range cover {
		fmt.Printf("  ⮕ %s\n", e)
	}
	effectsNote := ""
	if ix.CrossesHighFanOut(forward) {
		// The FORWARD reach crossed a HighFanOut dispatch seam — the context-insensitive
		// graph fans the single dispatch site onto EVERY callee that flows to it, so this
		// effect set may include sibling-closure effects past the seam, not just this
		// function's. An upper bound, mirroring the cover-side disclosure above and the MCP
		// impact card's EffectsOverApprox field (both render graph.OverApproxEffectsNote —
		// one source of truth), so the CLI and MCP lenses disclose this identically.
		effectsNote = graph.OverApproxEffectsNote
	}
	fmt.Printf("reachable external effects: %d%s\n", len(effects), effectsNote)
	for _, e := range effects {
		marker := ""
		if e.IsDynamic() {
			marker = "  ⚠ unresolved (soundness frontier)"
		}
		fmt.Printf("  %s [%s]%s\n", strings.TrimPrefix(e.To, "boundary:"), e.Boundary, marker)
	}
	if bs := ix.BlindSpotsAt(fqn); len(bs) > 0 {
		// Surface the per-spot detail AND the human/AI annotation context, so reach and
		// ground agree on what a blind spot means rather than reach showing a thinner
		// view (§21.C). The count carries the ExternalBoundaryCall signal/noise split so a
		// bare "N" is readable. Annotations are keyed by (site, kind); print a seam's
		// context once, under its first row, exactly as ground.Render does.
		fmt.Printf("blind spots on this function: %d%s\n", len(bs), graph.EBCTierNote(bs))
		graph.WriteBlindSpots(os.Stdout, bs, ix.DistinctAnnotationsAt(bs), func(b graph.BlindSpot) string {
			row := "  ⚠ " + b.Kind + " — " + b.Detail
			if b.Severity != "" {
				row += " [" + b.Severity + "]"
			}
			return row
		})
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
			// A broker named by two policies is only a problem if they DISAGREE:
			// the bus is one thing, so two different guarantees for it have no
			// single source. An identical re-declaration is harmless (mirrors the
			// mcp chains lens, which conflicts only on differing values).
			if existing, dup := brokers[name]; dup && existing != b {
				return fmt.Errorf("broker %q declared differently by more than one --policy; the bus guarantee must have a single source", name)
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
	expect, hasExpect, args := takeValueFlag(args, "--expect", "-expect")
	fs := flag.NewFlagSet("fitness", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: groundwork fitness <policy.json> <graph.json> [--expect <sha>] [--sarif]")
	}
	p, err := policy.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	g, err := graph.LoadFile(fs.Arg(1))
	if err != nil {
		return err
	}
	if err := verifyGateStamp(g, expect, hasExpect); err != nil {
		return err
	}
	res := fitness.Check(p, graph.NewIndex(g))

	// Provenance first: a green fitness pass is only as strong as the substrate it
	// was computed on, so the verdict states which call-graph algorithm produced
	// the graph it judged (R3), flags a policy-vs-graph algorithm mismatch (§9),
	// and, when the graph was built with `--reclaim`, that the verdict leaned on
	// edges recovered at a dispatch seam (R9). All three ride the substrate line as
	// caveats — a disclosure, never a gate finding, so the same signal cannot leak
	// into a review/verify finding diff (which judges over the same fitness.Check).
	// They are computed once here and surfaced on BOTH output paths: the SARIF path
	// carries them as notifications so an unsound-substrate pass cannot annotate a
	// PR as a clean green run.
	caveats := append([]string{}, g.Caveats...)
	if mc := graph.SubstrateMismatchCaveat(p.Substrate, g.Algo); mc != "" {
		caveats = append(caveats, mc)
	}
	if rc := g.ReclaimCaveat(); rc != "" {
		caveats = append(caveats, rc)
	}

	if sarif {
		b, err := toSARIF(res.Findings, caveats)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		if !res.OK() {
			return verdictf("%d invariant violation(s)", len(res.Violations()))
		}
		return nil
	}
	fmt.Print(graph.ProvenanceLine(g.Algo, caveats))
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

// cmdReviewTriage is the PROTOTYPE reviewer-triage surface: it partitions the MR's
// changed functions into vouched (fully resolved — complete evidence shown) and focus
// (touches a blind spot — look here). No policy, no verdict, no stamp gate: it is a
// comprehension aid, not a gate, so it never exits non-zero on content.
func cmdReviewTriage(args []string) error {
	asJSON, rest := takeFlag(args, "--json", "-json")
	asMermaid, rest := takeFlag(rest, "--mermaid", "-mermaid")
	asSummary, rest := takeFlag(rest, "--summary", "-summary")
	full, rest := takeFlag(rest, "--full", "-full")
	maxArg, _, rest := takeValueFlag(rest, "--max-nodes", "-max-nodes")
	policyArg, hasPolicy, rest := takeValueFlag(rest, "--policy", "-policy")
	if len(rest) != 2 {
		return fmt.Errorf("usage: groundwork review-triage <base-graph.json> <branch-graph.json> [--json | --mermaid | --summary] [--policy <policy.json>] [--full] [--max-nodes N]")
	}
	if b2i(asJSON)+b2i(asMermaid)+b2i(asSummary) > 1 {
		return fmt.Errorf("review-triage: choose at most one of --json, --mermaid, --summary")
	}
	opts := reviewtriage.Options{Full: full}
	if maxArg != "" {
		n, err := strconv.Atoi(maxArg)
		// Require a POSITIVE budget: 0 would otherwise pass and then be silently treated
		// as the default by Options.budget() (which maps 0 → default), ignoring the flag.
		if err != nil || n < 1 {
			return fmt.Errorf("--max-nodes: want a positive integer, got %q", maxArg)
		}
		opts.MaxNodes = n
	}
	// An optional policy enables the per-route write-movement section (it is what
	// fitness.RouteWrites needs to enumerate routes/roots); without it the rest still works.
	var p *policy.Policy
	if hasPolicy {
		loaded, err := policy.Load(policyArg)
		if err != nil {
			return err
		}
		p = loaded
	}
	base, err := graph.LoadFile(rest[0])
	if err != nil {
		return err
	}
	branch, err := graph.LoadFile(rest[1])
	if err != nil {
		return err
	}
	rep := reviewtriage.Build(base, branch, p)
	switch {
	case asJSON:
		b, err := canonjson.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	case asMermaid:
		fmt.Print(rep.RenderMermaid(opts))
	case asSummary:
		fmt.Print(rep.RenderSummary(opts))
	default:
		fmt.Print(rep.RenderMarkdown(opts))
	}
	return nil
}

// b2i is 1 for true, 0 for false — for counting how many mutually-exclusive flags are set.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cmdReview computes the base-vs-branch MR review artifact. With --json it emits
// the canonical artifact (the form a verifier reads); otherwise the human report.
// A BLOCK verdict exits non-zero so the same command can back a CI gate.
func cmdReview(args []string) error {
	asJSON, rest := takeFlag(args, "--json", "-json")
	expect, hasExpect, rest := takeValueFlag(rest, "--expect", "-expect")
	if len(rest) != 3 {
		return fmt.Errorf("usage: groundwork review <policy.json> <base-graph.json> <branch-graph.json> [--expect <sha>] [--json]")
	}
	p, base, branch, err := loadReviewInputs(rest[0], rest[1], rest[2])
	if err != nil {
		return err
	}
	if err := verifyGateStamp(branch, expect, hasExpect); err != nil {
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
	expect, hasExpect, rest := takeValueFlag(rest, "--expect", "-expect")
	corpusDir, hasCorpus, rest := takeValueFlag(rest, "--corpus", "-corpus")
	captureArg, hasCapture, rest := takeValueFlag(rest, "--capture", "-capture")
	asJSON, rest := takeFlag(rest, "--json", "-json")
	if len(rest) != 3 {
		return fmt.Errorf("usage: groundwork verify <policy.json> <base-graph.json> <branch-graph.json> [--scope pkg,pkg] [--expect <sha>] [--corpus <dir> [--capture production|integration]] [--json]")
	}
	// --capture is a reconciliation input for the corpus's self-described grade
	// (§12.6); asserting it without a corpus is a silent no-op of a trust assertion,
	// so fail loud rather than discard it.
	if hasCapture && !hasCorpus {
		return fmt.Errorf("--capture %q requires --corpus (it asserts the fidelity grade of a behavioral corpus)", captureArg)
	}
	// Only production/integration may be asserted (capture.AssertableGrade — the same
	// ONE source the MCP server validates against); an unrecognized grade is refused
	// here, never laundered into a silent CAPTURE-UNTRUSTED downgrade in the ladder.
	if hasCapture && !capture.AssertableGrade(captureArg) {
		return fmt.Errorf("--capture: grade must be %q or %q, got %q", capture.CaptureProduction, capture.CaptureIntegration, captureArg)
	}
	scope := splitComma(scopeArg)
	p, base, branch, err := loadReviewInputs(rest[0], rest[1], rest[2])
	if err != nil {
		return err
	}
	if err := verifyGateStamp(branch, expect, hasExpect); err != nil {
		return err
	}

	// A committed behavioral corpus (the *.golden.json snapshots, §14-B) optionally
	// feeds the impeachment gate (§9). It is COMMITTED by construction here —
	// loaded from versioned files, byte-identical run-to-run — so OriginCommitted is
	// sound; a live corpus is never a verify input (it would make the gate
	// non-deterministic, §13 crack #2). Disclosed always; blocks only under
	// impeachment_gate.gate (observe-first).
	var opts []review.GateOption
	if hasCorpus {
		blockers, notes, err := committedImpeachmentBlockers(p, branch, corpusDir, captureArg)
		if err != nil {
			return err
		}
		// WithImpeachmentNotes carries the binding disclosures so a corpus that did
		// not bind (VERSION-SKEW / CAPTURE-UNTRUSTED) cannot read as a clean PASS —
		// the silent-PASS this path previously had.
		opts = append(opts, review.WithImpeachment(blockers), review.WithImpeachmentNotes(notes))
	}
	g := review.Gate(p, base, branch, scope, opts...)

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
	expect, hasExpect, args := takeValueFlag(args, "--expect", "-expect")
	fs := flag.NewFlagSet("verify-artifact", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 4 {
		return fmt.Errorf("usage: groundwork verify-artifact <artifact.json> <policy.json> <base-graph.json> <branch-graph.json> [--expect <sha>]")
	}
	art, err := review.LoadArtifact(fs.Arg(0))
	if err != nil {
		return err
	}
	p, base, branch, err := loadReviewInputs(fs.Arg(1), fs.Arg(2), fs.Arg(3))
	if err != nil {
		return err
	}
	if err := verifyGateStamp(branch, expect, hasExpect); err != nil {
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

// committedImpeachmentBlockers audits the branch graph against a committed
// behavioral corpus and returns the gate-blocking impeachments (§9). The corpus is
// stampless (committed), so its code identity is the GATED commit — the branch
// graph's own stamp (§14-E: "the committed corpus takes the gated SHA"). The
// capture fidelity (§12.6, the one human-asserted rung) is reconciled from two
// sources: the grade each golden SELF-DESCRIBES (the committed corpus carries its
// own "production"/"integration" capture grade, written into the golden) and the
// optional caller-asserted `capture` flag. resolveCaptureProvenance fails CLOSED on
// a contradiction — a caller asserting a grade the corpus does not carry yields no
// established grade, capping the capture-fidelity rung at CAPTURE-UNTRUSTED so no
// candidate promotes. Only an established production/integration grade can promote a
// candidate to a gating impeachment; a corpus that self-describes neither (and no
// caller assertion) never blocks. GateBlockers additionally fences to a committed
// corpus, so a live trace can never reach here.
// It also returns the binding disclosures (Resolution.BindingDisclosures): when the
// corpus produced candidates but none bound (VERSION-SKEW / CAPTURE-UNTRUSTED), the
// gate must say so rather than pass silently — a non-binding corpus that reads as a
// clean PASS is the trust-corroding default this surfaces.
func committedImpeachmentBlockers(p *policy.Policy, branch *graph.Graph, dir, capture string) ([]impeach.GateFinding, []string, error) {
	traces, err := loadCommittedCorpus(dir)
	if err != nil {
		return nil, nil, err
	}
	ix := graph.NewIndex(branch)
	prov := impeach.Provenance{TraceIdentity: branch.Stamp, Capture: capture}
	r := impeach.Audit(p.Service, ix, traces, prov)
	res := impeach.Resolve(r, ix, p.MustNotReach, impeach.OriginCommitted)
	return res.GateBlockers(), res.BindingDisclosures(), nil
}

// loadCommittedCorpus reads every committed canonical-trace golden (*.golden.json)
// at or below dir. It fails CLOSED: it walks the tree RECURSIVELY (a flat glob
// would silently skip goldens nested under dir — a dropped trace could hide an
// impeachment, a fail-OPEN gate), and a malformed golden is an error, never a
// silently skipped trace. Paths are collected in WalkDir's lexical order; the
// corpus digest is order-independent regardless (§5).
func loadCommittedCorpus(dir string) ([]*ir.CanonicalTrace, error) {
	var paths []string
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".golden.json") {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no *.golden.json traces found under %s", dir)
	}
	traces := make([]*ir.CanonicalTrace, 0, len(paths))
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		t, err := ir.Load(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		traces = append(traces, t)
	}
	return traces, nil
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
	// Disclose the one ratchet config whose soundness backstop is off — gating the
	// effect ratchet without gating blind_spot_ratchet leaves the dynamic-laundering
	// escape open. Advisory (exit 0); same wording as fitness via the shared helper.
	if c := p.EffectRatchetCouplingCaution(); c != "" {
		fmt.Printf("  ⚠ %s\n", c)
	}
	return nil
}
