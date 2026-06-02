// Command flowmap is the CLI for the flowmap verification system: the static
// subcommands `boundary` (generate or --check the gated boundary contract) and
// `graph` (the non-gated call-graph view); `diff` (the structural change set
// between two canonical traces); and `coverage` (boundary effects no flow
// exercises).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/coverage"
	"github.com/jyang234/golang-code-graph/internal/diff"
	"github.com/jyang234/golang-code-graph/internal/ingest"
	"github.com/jyang234/golang-code-graph/internal/otlpjson"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/boundary"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/ir"
)

// version is overridden at build time via -ldflags "-X main.version=...".
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
		fmt.Println("flowmap", version)
		return nil
	case "boundary":
		return cmdBoundary(args[1:])
	case "graph":
		return cmdGraph(args[1:])
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
	dir := dirArg(fs)

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := dirArg(fs)

	res, err := analyze.Analyze(dir)
	if err != nil {
		return err
	}
	g, err := graphio.Build(res, *entry)
	if err != nil {
		return err
	}
	b, err := g.Marshal()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
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
	dir := dirArg(fs)
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

// cmdIngest reads an OTLP/JSON trace export (a collector file exporter's output),
// groups it into per-flow, per-service fragments, canonicalizes each, and reports
// the boundary effects the e2e run actually exercised. With --service-dir it also
// prints the coverage delta against that service's static boundary contract.
//
// It is the non-gated stage-1 view (post-hoc design §6): it always exits 0, so a
// truncated or partial capture is reported, never a build failure.
func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	serviceDir := fs.String("service-dir", "", "service source dir; show the coverage delta against its boundary contract")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: flowmap behavior ingest <traces-file-or-dir> [--service-dir D]")
	}

	spans, err := otlpjson.DecodePath(fs.Arg(0))
	if err != nil {
		return err
	}
	flows := ingest.Group(spans)
	if len(flows) == 0 {
		fmt.Printf("ingest: %d span(s), none tagged %s — nothing to map\n", len(spans), ingest.FlowKey)
		return nil
	}

	fmt.Printf("ingest: %d flow fragment(s) from %d span(s):\n", len(flows), len(spans))
	exercised := map[string]bool{}
	traces := make([]*ir.CanonicalTrace, 0, len(flows))
	for _, fc := range flows {
		tr, err := canon.Canonicalize(fc.Flow, nil)
		if err != nil {
			fmt.Printf("  - %-24s [%-10s] skipped: %v\n", fc.Slug, fc.Service, err)
			continue
		}
		effects := map[string]bool{}
		boundaryOps(tr.Root, effects)
		note := ""
		if fc.Synthesized {
			note = " (synthetic root — no inbound entry span)"
		}
		fmt.Printf("  - %-24s [%-10s] %d boundary effect(s)%s\n", fc.Slug, fc.Service, len(effects), note)
		for k := range effects {
			exercised[k] = true
		}
		traces = append(traces, tr)
	}

	if len(exercised) > 0 {
		fmt.Printf("\nboundary effects exercised (%d):\n", len(exercised))
		for _, k := range sortedKeys(exercised) {
			fmt.Println("  " + k)
		}
	}

	if *serviceDir != "" {
		c, err := boundary.Generate(*serviceDir)
		if err != nil {
			return err
		}
		r := coverage.Delta(c, traces)
		if r.Empty() {
			fmt.Printf("\ncoverage: every boundary effect is exercised by the ingested flows\n")
		} else {
			fmt.Printf("\ncoverage: %d boundary effect(s) unexercised:\n", len(r.Unexercised))
			for _, e := range r.Unexercised {
				fmt.Printf("  [%s] %s\n", e.Category, e.Key)
			}
		}
	}
	return nil
}

// boundaryOps records the canonical op keys in a trace that name a boundary
// effect — a published/consumed event, an outbound HTTP/RPC dependency. These
// are the keys the coverage join speaks (plan [H2]); internal and DB ops are not
// boundary effects and are omitted.
func boundaryOps(s *ir.CanonicalSpan, into map[string]bool) {
	if s == nil {
		return
	}
	if isBoundaryOp(s.Op) {
		into[s.Op] = true
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			boundaryOps(m, into)
		}
	}
}

func isBoundaryOp(op string) bool {
	for _, p := range []string{"PUBLISH ", "CONSUME ", "HTTP ", "RPC "} {
		if strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

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

// dirArg returns the first positional argument, defaulting to the current
// directory.
func dirArg(fs *flag.FlagSet) string {
	if d := fs.Arg(0); d != "" {
		return d
	}
	return "."
}

func usage() {
	fmt.Println(`flowmap — Go microservice boundary & behavior verification

usage: flowmap <command> [flags] [dir]

commands:
  boundary [--check] [dir]   generate the gated boundary contract (--check: verify currency)
  graph [--entry R] [dir]    print the non-gated call-graph view
  diff <a.json> <b.json>     print the structural change set between two golden traces
  coverage [--flows D] [dir] boundary effects no committed flow exercises
  behavior ingest <traces>   map an OTLP/JSON trace export to boundary effects (post-hoc, non-gated)
  version                    print the flowmap version
  help                       show this message`)
}
