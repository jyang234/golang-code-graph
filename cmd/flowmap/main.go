// Command flowmap is the CLI for the flowmap verification system: the static
// subcommands `boundary` (generate or --check the gated boundary contract),
// `graph` (the non-gated call-graph view), and `frontier` (the A/B/B2/C
// classification of where static reachability stops — a measurement, not a gate);
// `diff` (the structural change set between two canonical traces); and `coverage`
// (boundary effects no flow exercises).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/internal/buildinfo"
	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/coverage"
	"github.com/jyang234/golang-code-graph/internal/diff"
	"github.com/jyang234/golang-code-graph/internal/golden"
	"github.com/jyang234/golang-code-graph/internal/ingest"
	"github.com/jyang234/golang-code-graph/internal/otlpjson"
	"github.com/jyang234/golang-code-graph/internal/render"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/boundary"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/syscontext"
	"github.com/jyang234/golang-code-graph/ir"
)

// version is overridden at build time via -ldflags "-X main.version=...".
// When unset, buildinfo.Version recovers the module/VCS stamp Go embeds so an
// installed binary still names itself (see internal/buildinfo).
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "flowmap:", err)
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
		fmt.Println("flowmap", buildinfo.Version(version))
		return nil
	case "boundary":
		return cmdBoundary(args[1:])
	case "graph":
		return cmdGraph(args[1:])
	case "frontier":
		return cmdFrontier(args[1:])
	case "diff":
		return cmdDiff(args[1:])
	case "coverage":
		return cmdCoverage(args[1:])
	case "behavior":
		return cmdBehavior(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (try `flowmap help`)", args[0])
	}
}

// cmdBoundary generates the gated boundary contract for a service directory. With
// --check it instead verifies the committed contract is current, exiting non-zero
// if it is stale — the currency gate.
func cmdBoundary(args []string) error {
	fs := flag.NewFlagSet("boundary", flag.ContinueOnError)
	check := fs.Bool("check", false, "verify the committed contract is current; non-zero exit if stale")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}

	c, err := boundary.Generate(dir)
	if err != nil {
		return err
	}
	path := boundary.ContractPath(dir)

	if *check {
		match, err := boundary.Check(dir, c)
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("boundary contract is stale: regenerate with `flowmap boundary %s` and commit %s", dir, path)
		}
		fmt.Println("boundary contract current:", path)
		return nil
	}

	if err := boundary.Write(dir, c); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

// cmdGraph prints the non-gated call-graph view, optionally scoped to one entry
// point with --entry.
func cmdGraph(args []string) error {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	entry := fs.String("entry", "", `scope to the subgraph reachable from this entry point (e.g. "POST /loan-application")`)
	stamp := fs.String("stamp", "", "identity stamp (e.g. the commit SHA) recorded in the graph; consumers can verify with --expect")
	algo := fs.String("algo", "", `call-graph algorithm: "rta" (default), "vta" (refines interface-dense dispatch — fewer spurious callees), "cha" (rootless fallback)`)
	reclaimFlag := fs.Bool("reclaim", false, "apply sound dispatch-seam reclaimers (opt-in; adds provenance-tagged edges that close the strict-server seam)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}

	opt, err := algoOption(*algo)
	if err != nil {
		return err
	}
	res, err := analyze.Analyze(dir, opt)
	if err != nil {
		return err
	}
	g, err := graphio.Build(res, *entry)
	if err != nil {
		return err
	}
	if *reclaimFlag {
		graphio.ApplyReclaimers(g, res)
	}
	// The stamp is caller-supplied, never derived: deriving it (from git HEAD,
	// a timestamp) would make the graph a function of more than the code and
	// break byte-identical regeneration. Unstamped output is byte-identical to
	// pre-stamp flowmap; CI passes --stamp "$GITHUB_SHA" explicitly.
	g.Stamp = *stamp
	b, err := g.Marshal()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// cmdFrontier classifies a service's static frontier (docs/design/frontier-
// instrumentation-plan.md, Phase 1): the deterministic A/B/B2/C inventory of every
// place reachability stops being able to answer, with the reclaimable share and
// the route attribution-loss ratio. It is a measurement view, not a gate — it
// never fails closed and never touches a verdict. Default output is the human
// summary; --json emits the full marker list as canonical JSON.
func cmdFrontier(args []string) error {
	fs := flag.NewFlagSet("frontier", flag.ContinueOnError)
	algo := fs.String("algo", "", `call-graph algorithm: "rta" (default), "vta" (refines interface-dense dispatch), "cha"`)
	asJSON := fs.Bool("json", false, "emit the full marker inventory as canonical JSON")
	reclaimFlag := fs.Bool("reclaim", false, "apply sound dispatch-seam reclaimers before classifying (shows the frontier with the seam closed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}

	opt, err := algoOption(*algo)
	if err != nil {
		return err
	}
	res, err := analyze.Analyze(dir, opt)
	if err != nil {
		return err
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		return err
	}
	if *reclaimFlag {
		graphio.ApplyReclaimers(g, res)
	}
	// Build already classified and embedded the frontier section; summarize it.
	rep := frontier.Summarize(g.Frontier, len(g.Entrypoints))
	rep.Algo = g.Algo // carry the call-graph algorithm into the --json provenance
	if *asJSON {
		b, err := canonjson.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	}
	fmt.Print(frontier.Render(dir, g.Algo, rep))
	return nil
}

// algoOption maps the --algo flag to a call-graph Options. The empty string and
// "rta" both select the default (RTA); only "vta" and "cha" deviate. An unknown
// value is rejected here with a friendly message rather than deferred to the
// builder's generic error.
func algoOption(s string) (callgraph.Options, error) {
	switch s {
	case "", "rta":
		return callgraph.Options{Algo: callgraph.AlgoRTA}, nil
	case "vta":
		return callgraph.Options{Algo: callgraph.AlgoVTA}, nil
	case "cha":
		return callgraph.Options{Algo: callgraph.AlgoCHA}, nil
	default:
		return callgraph.Options{}, fmt.Errorf("unknown --algo %q (want rta, vta, or cha)", s)
	}
}

// cmdDiff prints the structural, prioritized change set between two canonical
// golden traces (a = baseline, b = observed). It exits non-zero when the flows
// differ, so it can back a CI check, and is renderer-drift-immune because it
// diffs the IR, not the rendered view.
func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: flowmap diff <baseline.golden.json> <observed.golden.json>")
	}
	a, err := loadTrace(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := loadTrace(fs.Arg(1))
	if err != nil {
		return err
	}
	changes := diff.Diff(a, b)
	if len(changes) == 0 {
		fmt.Println("no behavioral changes")
		return nil
	}
	for _, c := range changes {
		fmt.Println(c.String())
	}
	return fmt.Errorf("%d behavioral change(s) detected", len(changes))
}

// cmdCoverage reports the boundary effects that no committed flow exercises — the
// delta between the static boundary (all reachable effects) and the union of
// behavioral snapshots (tested effects). It is informational (exit 0): coverage
// gaps are feedback, not a gate failure.
func cmdCoverage(args []string) error {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	flowsDir := fs.String("flows", "", "directory of committed *.golden.json snapshots (default: <dir>/testdata/flows)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}
	gdir := *flowsDir
	if gdir == "" {
		gdir = defaultFlowsDir(dir)
	}

	c, err := boundary.Generate(dir)
	if err != nil {
		return err
	}
	traces, err := loadGoldens(gdir)
	if err != nil {
		return err
	}

	// Zero flows is not full coverage — it is coverage of nothing. Saying "every
	// effect is exercised by 0 flow(s)" reads as a clean pass while having checked
	// nothing, the exact false-green this tool exists to avoid. Disclose it, and
	// point at the likely cause: a directory of post-hoc *.effects.json goldens,
	// which this in-process path (it joins *.golden.json canonical traces) does
	// not read (F7).
	if len(traces) == 0 {
		fmt.Printf("coverage: no *.golden.json flow snapshots in %s — checked nothing (this is not coverage)\n", gdir)
		if hint := otherGoldenHint(gdir); hint != "" {
			fmt.Print(hint)
		}
		return nil
	}

	r := coverage.Delta(c, traces)
	if r.Empty() {
		fmt.Printf("coverage: every boundary effect is exercised by %d flow(s)\n", len(traces))
		return nil
	}
	fmt.Printf("coverage: %d boundary effect(s) unexercised by %d flow(s):\n", len(r.Unexercised), len(traces))
	for _, e := range r.Unexercised {
		fmt.Printf("  [%s] %s\n", e.Category, e.Key)
	}
	return nil
}

// otherGoldenHint detects post-hoc goldens (*.effects.json) sitting in a flows
// directory that `coverage` found no *.golden.json in, and returns a one-line
// pointer to the right surface — so an empty in-process coverage run names the
// format mismatch instead of leaving the reader to guess.
func otherGoldenHint(dir string) string {
	if effects, _ := filepath.Glob(filepath.Join(dir, "*.effects.json")); len(effects) > 0 {
		return fmt.Sprintf("  hint: %s holds %d post-hoc *.effects.json golden(s); those are gated by `flowmap behavior ingest --flows-dir`, not `coverage`\n", dir, len(effects))
	}
	return ""
}

// cmdBehavior dispatches the behavioral subcommands. Today: `ingest`, the
// post-hoc out-of-process path.
func cmdBehavior(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowmap behavior ingest <traces> [flags]")
	}
	switch args[0] {
	case "ingest":
		return cmdIngest(args[1:])
	default:
		return fmt.Errorf("unknown behavior subcommand %q (try `flowmap behavior ingest`)", args[0])
	}
}

// ingestFragment is one canonicalized post-hoc flow fragment.
type ingestFragment struct {
	fc      ingest.FlowCapture
	trace   *ir.CanonicalTrace
	effects []string
}

// cmdIngest reads an OTLP/JSON trace export (a collector file exporter's output),
// groups it into per-flow, per-service fragments, and canonicalizes each. Its
// behavior depends on the flags:
//
//   - no --flows-dir (stage 1, non-gated): print the boundary effects the run
//     exercised; with --service-dir, also the coverage delta. Always exits 0.
//   - --flows-dir --update: rebase the post-hoc goldens (*.effects.json) and
//     their rendered views (*.flow.md) from the ingested traces. Exits 0.
//   - --flows-dir (gate): compare each committed golden to what was observed,
//     failing (non-zero) only on a NEW boundary effect (design D-PH3). A golden
//     with no capture this run is skipped, never silently passed (D-PH2).
func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	serviceDir := fs.String("service-dir", "", "service source dir; show the coverage delta against its boundary contract")
	flowsDir := fs.String("flows-dir", "", "directory of committed *.effects.json post-hoc goldens; enables the opt-in gate")
	update := fs.Bool("update", false, "with --flows-dir, rebase the post-hoc goldens and .flow.md views from the traces")
	renderDir := fs.String("render-dir", "", "write a cross-service <slug>.system.flow.md per flow here (any mode, non-gated)")
	root := fs.String("root", "", "with --render-dir, center the diagram on this service's subtree")
	merged := fs.Bool("merged", false, "with --render-dir, also write system.context.md: all flows merged into one service-interaction graph")
	choreography := fs.Bool("choreography", false, "with --merged, join publisher→subscriber on the event name instead of routing through a Bus node")
	contracts := fs.String("contracts", "", "with --merged, comma-separated service source dirs whose static boundary contracts overlay the graph (dashed = untested)")
	// Parse flags reorder-tolerantly: Go's flag package stops at the first
	// positional, so `ingest <traces> --flags` would silently ignore the flags.
	// Permute — parse, peel off the positional, parse the remainder — so flags may
	// appear on either side of the traces path.
	tracesPath, err := parsePermuted(fs, args)
	if err != nil {
		return err
	}
	if tracesPath == "" {
		return fmt.Errorf("usage: flowmap behavior ingest <traces-file-or-dir> [--flows-dir D] [--service-dir D] [--update] [--render-dir D [--root SVC] [--merged [--choreography] [--contracts dirs]]]")
	}
	if *root != "" && *renderDir == "" {
		return fmt.Errorf("--root requires --render-dir")
	}
	if (*merged || *choreography || *contracts != "") && *renderDir == "" {
		return fmt.Errorf("--merged/--choreography/--contracts require --render-dir")
	}
	if (*choreography || *contracts != "") && !*merged {
		return fmt.Errorf("--choreography/--contracts require --merged")
	}

	// Load the canon config from --service-dir (the contract/coverage anchor), so an
	// opt-in declared in the service's .flowmap.yaml — e.g. messagingShortHexIDs —
	// actually takes effect during ingest. Without a service dir, defaults apply.
	cfg, err := loadIngestConfig(*serviceDir)
	if err != nil {
		return err
	}

	spans, err := otlpjson.DecodePath(tracesPath)
	if err != nil {
		return err
	}
	flows := ingest.Group(spans)
	if len(flows) == 0 {
		fmt.Printf("ingest: %d span(s), none tagged %s — nothing to map\n", len(spans), ingest.FlowKey)
		return nil
	}

	fmt.Printf("ingest: %d flow fragment(s) from %d span(s):\n", len(flows), len(spans))
	var frags []ingestFragment
	for _, fc := range flows {
		tr, err := canon.Canonicalize(fc.Flow, cfg)
		if err != nil {
			fmt.Printf("  - %-24s [%-10s] skipped: %v\n", fc.Slug, fc.Service, err)
			continue
		}
		eff := ingest.BoundaryEffects(tr.Root)
		note := ""
		if fc.Synthesized {
			note = " (synthetic root — no inbound entry span)"
		}
		fmt.Printf("  - %-24s [%-10s] %d boundary effect(s)%s\n", fc.Slug, fc.Service, len(eff), note)
		frags = append(frags, ingestFragment{fc: fc, trace: tr, effects: eff})
	}

	// The cross-service views share one whole-flow canonicalization pass across
	// the per-flow diagrams, the merged graph, and the --update companion.
	var whole []wholeFlow
	if *renderDir != "" || (*update && *flowsDir != "") {
		whole = canonWholeFlows(spans, cfg)
	}
	// The cross-service view is independent of gating: emit it in any mode
	// (including non-gated stage 1) when a render dir is given.
	if *renderDir != "" {
		if err := writeSystemDiagrams(*renderDir, whole, *root); err != nil {
			return err
		}
		if *merged {
			if err := writeSystemContext(*renderDir, whole, *contracts, *choreography); err != nil {
				return err
			}
		}
	}

	switch {
	case *update && *flowsDir != "":
		if err := updateEffectGoldens(*flowsDir, frags); err != nil {
			return err
		}
		return writeSystemDiagrams(*flowsDir, whole, "") // the committed companion is the full choreography
	case *flowsDir != "":
		return gateEffectGoldens(*flowsDir, frags)
	default:
		printExercised(frags)
		if *serviceDir != "" {
			return printCoverage(*serviceDir, frags)
		}
		return nil
	}
}

// updateEffectGoldens rebases one golden + rendered view per fragment. The author
// reviews and commits only the flows they intend to gate (a committed golden is
// what opts a flow in).
func updateEffectGoldens(dir string, frags []ingestFragment) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fmt.Println()
	claimed := map[string]string{} // file stem -> "flow / service" that wrote it
	for _, fr := range frags {
		stemName := effectStem(fr.fc.Slug, fr.fc.Service)
		owner := fr.fc.Slug + " / " + fr.fc.Service

		// Two distinct flows whose slugs collide to the same filename would
		// silently overwrite each other (and the loser would be ungated on a later
		// run). Refuse rather than lose a golden — within this run...
		if prev, ok := claimed[stemName]; ok && prev != owner {
			return fmt.Errorf("golden filename collision: %q and %q both map to %s.effects.json; rename a flow or service to disambiguate", prev, owner, stemName)
		}
		claimed[stemName] = owner

		stem := filepath.Join(dir, stemName)
		// ...and against a committed golden from a previous run.
		if existing, err := ingest.LoadEffectGolden(stem + ".effects.json"); err == nil {
			if prev := existing.Flow + " / " + existing.Service; prev != owner {
				return fmt.Errorf("%s.effects.json already belongs to %q; refusing to overwrite it with %q (slug collision) — rename to disambiguate", stem, prev, owner)
			}
		}

		g := ingest.NewEffectGolden(fr.fc.Slug, fr.fc.Service, fr.effects)
		b, err := g.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(stem+".effects.json", b, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(stem+".flow.md", []byte(render.Mermaid(fr.trace)), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s.effects.json (+ .flow.md)\n", stem)
	}
	return nil
}

// gateEffectGoldens enforces every committed golden against what was observed,
// with no-new-effects semantics (D-PH3) and skip-on-no-capture (D-PH2).
func gateEffectGoldens(dir string, frags []ingestFragment) error {
	goldenPaths, err := filepath.Glob(filepath.Join(dir, "*.effects.json"))
	if err != nil {
		return err
	}
	observed := map[string]ingestFragment{}
	gatedKey := map[string]bool{}
	for _, fr := range frags {
		observed[key(fr.fc.Slug, fr.fc.Service)] = fr
	}

	fmt.Println()
	var added []string
	for _, gp := range goldenPaths {
		g, err := ingest.LoadEffectGolden(gp)
		if err != nil {
			return err
		}
		k := key(g.Flow, g.Service)
		gatedKey[k] = true
		fr, ok := observed[k]
		if !ok {
			// D-PH2: no capture for this golden this run — never silently pass it.
			fmt.Printf("  ! %s [%s]: no capture this run — skipped (not gated)\n", g.Flow, g.Service)
			continue
		}
		if fr.fc.Synthesized {
			// No clean inbound entry span: the fragment's completeness cannot be
			// established (it may be truncated, or span multiple traces), so the
			// flow is not gated this run rather than risk passing a partial capture
			// (D-PH2). canon's ErrIncomplete guard is unreachable here because the
			// post-hoc path always assembles a tree, so this is where completeness
			// is enforced for the gate.
			fmt.Printf("  ! %s [%s]: no inbound entry span (completeness unverifiable) — skipped (not gated)\n", g.Flow, g.Service)
			continue
		}
		newEffects, missing := ingest.CompareEffects(g.Effects, fr.effects)
		for _, m := range missing {
			fmt.Printf("  ~ %s [%s]: golden effect not exercised this run: %s (coverage, not a failure)\n", g.Flow, g.Service, m)
		}
		for _, a := range newEffects {
			added = append(added, fmt.Sprintf("[CONTRACT] ADDED %s  (flow %q, service %q)", a, g.Flow, g.Service))
		}
	}

	// Observed fragments without a committed golden are informational, not gated.
	for _, fr := range frags {
		if !gatedKey[key(fr.fc.Slug, fr.fc.Service)] {
			fmt.Printf("  · %s [%s]: ungated (run --update to snapshot and gate it)\n", fr.fc.Slug, fr.fc.Service)
		}
	}

	if len(added) > 0 {
		fmt.Println()
		for _, line := range added {
			fmt.Println("  " + line)
		}
		return fmt.Errorf("%d new boundary effect(s) not in the committed golden; review and run --update if intended", len(added))
	}
	fmt.Println("\nbehavioral gate: no new boundary effects")
	return nil
}

// writeSystemDiagrams emits one cross-service <slug>.system.flow.md per flow: the
// whole-flow choreography across every service the flow touched (the diagram
// unit, design D-PH1), distinct from the per-service gated artifacts. It is a
// view — never gated — so a fragment with no clean entry (synthesized root) is
// rendered best-effort with a note rather than skipped.
// wholeFlow is one flow's canonicalized cross-service tree, computed once and
// shared by every cross-service view.
type wholeFlow struct {
	slug        string
	synthesized bool
	trace       *ir.CanonicalTrace
}

// canonWholeFlows assembles and canonicalizes each flow's whole-flow tree once,
// so the per-flow diagrams, the merged graph, and the --update companion don't
// repeat the work.
func canonWholeFlows(spans []capture.Span, cfg *config.Config) []wholeFlow {
	var out []wholeFlow
	for _, wf := range ingest.WholeFlows(spans) {
		tr, err := canon.Canonicalize(wf.Flow, cfg)
		if err != nil {
			fmt.Printf("  - %-24s cross-service view skipped: %v\n", wf.Slug, err)
			continue
		}
		out = append(out, wholeFlow{slug: wf.Slug, synthesized: wf.Synthesized, trace: tr})
	}
	return out
}

func writeSystemDiagrams(dir string, flows []wholeFlow, rootSvc string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, wf := range flows {
		stem := filepath.Join(dir, golden.Slug(wf.slug)+".system")
		md := render.SystemMermaid(wf.trace)
		if rootSvc != "" {
			out, ok := render.SystemMermaidRootedAt(wf.trace, rootSvc)
			if !ok {
				fmt.Printf("  - %-24s service %q not in this flow — skipped\n", wf.slug, rootSvc)
				continue
			}
			md = out
			stem = filepath.Join(dir, golden.Slug(wf.slug)+"."+golden.Slug(rootSvc)+".system")
		}
		if err := os.WriteFile(stem+".flow.md", []byte(md), 0o644); err != nil {
			return err
		}
		note := ""
		if rootSvc == "" && wf.synthesized {
			note = " (no single entry — best-effort)"
		}
		fmt.Printf("wrote %s.flow.md (cross-service view%s)\n", stem, note)
	}
	return nil
}

// writeSystemContext merges every captured flow into one deduplicated
// service-interaction graph (system.context.md), optionally overlaying the
// static boundary contracts of the given service dirs (dashed = can-happen but
// no flow exercised it). It is a non-gated view.
func writeSystemContext(dir string, flows []wholeFlow, contractDirs string, choreography bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	traces := make([]*ir.CanonicalTrace, 0, len(flows))
	for _, wf := range flows {
		traces = append(traces, wf.trace)
	}

	var statics []syscontext.Contract
	for _, d := range splitList(contractDirs) {
		c, err := boundary.Generate(d)
		if err != nil {
			// A contract overlay is a non-gated view; a load failure must not abort
			// the ingest (and, when combined with --flows-dir, must not fail the
			// gate). Warn and skip the bad contract.
			fmt.Printf("  - contract %s: skipped: %v\n", d, err)
			continue
		}
		statics = append(statics, contractToSyscontext(c))
	}

	g := syscontext.Build(traces, statics, syscontext.Options{Choreography: choreography})
	path := filepath.Join(dir, "system.context.md")
	if err := os.WriteFile(path, []byte(render.SystemGraph(g)), 0o644); err != nil {
		return err
	}
	overlay := ""
	if len(statics) > 0 {
		overlay = fmt.Sprintf(" + %d contract(s) overlaid", len(statics))
	}
	fmt.Printf("wrote %s (%d node(s), %d edge(s)%s)\n", path, len(g.Nodes), len(g.Edges), overlay)
	return nil
}

// contractToSyscontext flattens a static boundary contract into the neutral form
// the system-context builder consumes, so syscontext stays free of the static
// analyzer's dependencies.
func contractToSyscontext(c *boundary.Contract) syscontext.Contract {
	sc := syscontext.Contract{Service: c.Service}
	for _, e := range c.Published {
		sc.Published = append(sc.Published, e.Event)
	}
	for _, e := range c.Consumed {
		sc.Consumed = append(sc.Consumed, e.Event)
	}
	for _, d := range c.ExternalDeps {
		sc.Deps = append(sc.Deps, syscontext.Dep{Peer: d.Peer, Kind: d.Kind})
	}
	return sc
}

func splitList(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// printExercised lists the union of boundary effects across all fragments.
func printExercised(frags []ingestFragment) {
	exercised := map[string]bool{}
	for _, fr := range frags {
		for _, e := range fr.effects {
			exercised[e] = true
		}
	}
	if len(exercised) == 0 {
		return
	}
	fmt.Printf("\nboundary effects exercised (%d):\n", len(exercised))
	for _, k := range sortedKeys(exercised) {
		fmt.Println("  " + k)
	}
}

// printCoverage shows the delta against a service's static boundary contract.
func printCoverage(serviceDir string, frags []ingestFragment) error {
	c, err := boundary.Generate(serviceDir)
	if err != nil {
		return err
	}
	traces := make([]*ir.CanonicalTrace, 0, len(frags))
	for _, fr := range frags {
		traces = append(traces, fr.trace)
	}
	r := coverage.Delta(c, traces)
	if r.Empty() {
		fmt.Printf("\ncoverage: every boundary effect is exercised by the ingested flows\n")
		return nil
	}
	fmt.Printf("\ncoverage: %d boundary effect(s) unexercised:\n", len(r.Unexercised))
	for _, e := range r.Unexercised {
		fmt.Printf("  [%s] %s\n", e.Category, e.Key)
	}
	return nil
}

// effectStem is the per-(flow,service) golden file stem, e.g.
// "post_loan_application.loansvc" (design D-PH4 naming).
func effectStem(flow, service string) string {
	return golden.Slug(flow) + "." + golden.Slug(service)
}

func key(flow, service string) string { return flow + "\x00" + service }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// defaultFlowsDir picks the conventional goldens location: <dir>/testdata/flows
// (flow tests at the service root), or <dir>/flows/testdata/flows (flow tests in
// a flows/ package, where `go test` writes goldens package-relative). The first
// directory that exists wins; otherwise the root convention is returned so the
// error names a sensible path.
func defaultFlowsDir(dir string) string {
	root := filepath.Join(dir, "testdata", "flows")
	nested := filepath.Join(dir, "flows", "testdata", "flows")
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		if _, err := os.Stat(root); err != nil {
			return nested
		}
	}
	return root
}

// loadGoldens loads every *.golden.json in dir as a canonical trace.
func loadGoldens(dir string) ([]*ir.CanonicalTrace, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.golden.json"))
	if err != nil {
		return nil, err
	}
	traces := make([]*ir.CanonicalTrace, 0, len(matches))
	for _, m := range matches {
		t, err := loadTrace(m)
		if err != nil {
			return nil, err
		}
		traces = append(traces, t)
	}
	return traces, nil
}

// loadTrace reads a canonical golden IR from path.
func loadTrace(path string) (*ir.CanonicalTrace, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := ir.Load(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// loadIngestConfig reads the canon config (.flowmap.yaml) from the service dir — the
// same anchor the boundary contract and coverage delta use — or returns nil defaults
// when no service dir is given. This is the only path by which a service's config
// (e.g. the messagingShortHexIDs opt-in) reaches behavior ingest.
func loadIngestConfig(serviceDir string) (*config.Config, error) {
	if serviceDir == "" {
		return nil, nil
	}
	return config.LoadDir(serviceDir)
}

// parsePermuted parses fs allowing flags and a single positional in any order
// (Go's flag package otherwise stops at the first positional). It returns the lone
// positional, or "" if there is none, and errors if more than one is given.
func parsePermuted(fs *flag.FlagSet, args []string) (string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch len(positional) {
	case 0:
		return "", nil
	case 1:
		return positional[0], nil
	default:
		return "", fmt.Errorf("expected one traces path, got %d: %v", len(positional), positional)
	}
}

// dirArg returns the first positional argument, defaulting to the current
// directory.
// dirArg returns the positional directory argument (defaulting to "."). It errors
// if a flag was placed AFTER the directory: Go's flag package stops parsing at the
// first non-flag token, so `flowmap <cmd> <dir> --x` would otherwise silently drop
// --x. Surfacing it turns a silent no-op (e.g. an ignored --reclaim that looks like
// success) into a clear message.
func dirArg(fs *flag.FlagSet) (string, error) {
	rest := fs.Args()
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			return "", fmt.Errorf("%s: flags must come before the directory argument (got %q)", fs.Name(), a)
		}
	}
	if len(rest) > 0 {
		return rest[0], nil
	}
	return ".", nil
}

func usage() {
	fmt.Println(`flowmap — Go microservice boundary & behavior verification

usage: flowmap <command> [flags] [dir]

commands:
  boundary [--check] [dir]   generate the gated boundary contract (--check: verify currency)
  graph [--entry R] [--algo A] [--reclaim] [dir]  print the non-gated call-graph view (--reclaim closes sound dispatch seams)
  frontier [--algo A] [--reclaim] [--json] [dir]  classify the static frontier (A/B/B2/C) — measurement, not a gate
  diff <a.json> <b.json>     print the structural change set between two golden traces
  coverage [--flows D] [dir] boundary effects no committed flow exercises
  behavior ingest <traces>   map an OTLP/JSON trace export to boundary effects
                             [--service-dir D] coverage delta; [--flows-dir D] gate
                             on committed *.effects.json (--update to rebase)
  version                    print the flowmap version
  help                       show this message`)
}
