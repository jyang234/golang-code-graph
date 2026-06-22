// Command flowmap is the CLI for the flowmap verification system: the static
// subcommands `boundary` (generate or --check the gated boundary contract),
// `graph` (the non-gated call-graph view), and `frontier` (the A/B/B2/C
// classification of where static reachability stops — a measurement, not a gate);
// `diff` (the structural change set between two canonical traces); and `coverage`
// (boundary effects no flow exercises).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	"github.com/jyang234/golang-code-graph/internal/static/schemadrift"
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
	case "schema-drift":
		return cmdSchemaDrift(args[1:])
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
// point with --entry. Default output is canonical JSON; --mermaid renders the same
// graph as a human-readable flowchart (a view, never gated) for review.
func cmdGraph(args []string) error {
	fs := flag.NewFlagSet("graph", flag.ContinueOnError)
	entry := fs.String("entry", "", `scope to the subgraph reachable from this entry point (e.g. "POST /loan-application")`)
	stamp := fs.String("stamp", "", "identity stamp (e.g. the commit SHA) recorded in the graph; consumers can verify with --expect")
	algo := fs.String("algo", "", `call-graph algorithm: "rta" (default), "vta" (refines interface-dense dispatch — fewer spurious callees), "cha" (rootless fallback)`)
	reclaimFlag := fs.Bool("reclaim", false, "apply sound dispatch-seam reclaimers (opt-in; adds provenance-tagged edges that close the strict-server seam)")
	reclaimSQLFlag := fs.Bool("reclaim-sql", false, "apply the SQL const-accumulation label reclaimer (opt-in; recovers verbs from constant-fragment SQL builders, tagging them via=sql-constfold)")
	reclaimTopicFlag := fs.Bool("reclaim-topic", false, "apply the bus const-topic label reclaimer (opt-in; recovers PUBLISH/CONSUME targets from constant-set topics, tagging them via=topic-constfold)")
	asMermaid := fs.Bool("mermaid", false, "render the graph as a human-readable Mermaid flowchart instead of JSON (a view, never gated); scope with --entry")
	showPlumbing := fs.Bool("show-plumbing", false, "with --mermaid, include low-salience plumbing nodes (tier 3: telemetry, compute-only closures) instead of collapsing them")
	allBlindSpots := fs.Bool("all-blind-spots", false, "with --mermaid, draw every blind-spot/frontier disclosure node (trivial boundaries and those orphaned onto collapsed plumbing) instead of rolling the plumbing-tier ones into a counted header note; restores the full honesty channel without un-collapsing plumbing nodes")
	diffBase := fs.String("diff", "", "with --mermaid or --rollup, render the delta from this BASE graph JSON to the analyzed branch (added/removed elements; --rollup splits code vs disclosure); a view, never a gate")
	rootAt := fs.String("root", "", `with --mermaid, scope to one entry point at RENDER time (e.g. "POST /loan-application") — unlike --entry this keeps the frontier markers in the per-handler view`)
	maxNodes := fs.Int("max-nodes", 300, "with --mermaid, cap how many nodes a diagram draws; above the cap it renders an index of entry points to --root at instead of an illegible hairball (0 = uncapped)")
	rollup := fs.String("rollup", "", `emit a component-level (C3) rollup grouping nodes by package: "package". Default output is the rollup JSON; with --mermaid it renders the component flowchart, with --diff BASE the component delta (code-vs-disclosure split)`)
	rollupBands := fs.Bool("rollup-bands", false, "with --rollup --mermaid, group the component boxes into architectural BAND lanes (transport/application/provisioning/storage/infrastructure/tests, read from the package name) with the composition root drawn outside the lanes; a view grouping, never a gate (no-op without --mermaid)")
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
	g, err := graphio.Build(res, *entry, reclaimOpts(*reclaimSQLFlag, *reclaimTopicFlag)...)
	if err != nil {
		return err
	}
	warnSkippedAnnotations(os.Stderr, g)
	if *reclaimFlag {
		graphio.ApplyReclaimers(g, res)
	}
	if *rollup != "" {
		return cmdGraphRollup(*rollup, g, *asMermaid, *diffBase, *rootAt, *entry, *rollupBands)
	}
	if *asMermaid {
		// The Mermaid flowchart is a deterministic view of the graph, so it carries
		// no stamp/tool provenance (those gate-adjacent fields ride the JSON only).
		// Collapse tier-3 plumbing by default; --show-plumbing renders the full graph.
		maxTier := 2
		if *showPlumbing {
			maxTier = 0
		}
		opts := graphio.MermaidOptions{MaxTier: maxTier, MaxNodes: *maxNodes, ShowAllBlindSpots: *allBlindSpots}
		if *rootAt != "" && *diffBase != "" {
			return fmt.Errorf("graph --root and --diff are mutually exclusive: --root renders one handler's reach, --diff renders a base→branch delta; a rooted diff is not supported")
		}
		if *rootAt != "" {
			// Render-time scoping needs the UNSCOPED graph so the frontier section is
			// present; --entry would have scoped it at build time and dropped frontier.
			if *entry != "" {
				return fmt.Errorf("graph --root and --entry are mutually exclusive: --root scopes at render time over the full graph (keeping frontier markers), --entry scopes the build (dropping them)")
			}
			out, ok := g.MermaidRootedAt(*rootAt, opts)
			if !ok {
				return fmt.Errorf("graph --root %q: no unique entry point or function matches in this graph (no match, or an ambiguous route prefix)", *rootAt)
			}
			_, err = os.Stdout.WriteString(render.Fence(out))
			return err
		}
		if *diffBase != "" {
			base, err := loadGraphJSON(*diffBase)
			if err != nil {
				return fmt.Errorf("--diff base graph: %w", err)
			}
			// An empty base is NOT refused: a new service (or one absent from the base
			// branch) legitimately has an empty base graph, and an all-added diff is the
			// correct answer. MermaidDiff discloses the empty base in a caveat so the
			// "everything added" reading is unambiguous (new service vs wrong --diff base).
			// A truly malformed base is already rejected by loadGraphJSON's strict decode.
			_, err = os.Stdout.WriteString(render.Fence(graphio.MermaidDiff(base, g, opts)))
			return err
		}
		_, err = os.Stdout.WriteString(render.Fence(g.Mermaid(opts)))
		return err
	}
	// The stamp is caller-supplied, never derived: deriving it (from git HEAD,
	// a timestamp) would make the graph a function of more than the code and
	// break byte-identical regeneration. Unstamped output is byte-identical to
	// pre-stamp flowmap; CI passes --stamp "$GITHUB_SHA" explicitly.
	g.Stamp = *stamp
	// The tool version is the one provenance dimension the caller cannot supply —
	// only this binary knows which build it is — so it IS derived here, at the CLI
	// boundary rather than in graphio.Build (which stays pure: the determinism test
	// and Build-built goldens must not vary by producing binary). It lets groundwork
	// flag a base/branch built by two flowmap versions, whose "same code → same
	// graph" guarantee holds only within one tool version (R11). regen.sh strips it
	// so the committed goldens regenerate byte-identically, the unstamped convention.
	g.Tool = buildinfo.Version(version)
	b, err := g.Marshal()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// cmdGraphRollup emits the component-level (C3) rollup of g: the package grouping,
// the component→component dependencies, and the external-system effects (resolved +
// disclosed). Default output is canonical JSON; --mermaid renders the flowchart;
// --diff BASE renders the component delta. The rollup is a VIEW like --mermaid, so it
// carries no stamp/tool provenance (those gate-adjacent fields ride the graph JSON).
func cmdGraphRollup(kind string, g *graphio.Graph, asMermaid bool, diffBase, rootAt, entry string, bands bool) error {
	if kind != "package" {
		return fmt.Errorf(`graph --rollup: only "package" is supported, got %q`, kind)
	}
	if rootAt != "" {
		return fmt.Errorf("graph --rollup and --root are mutually exclusive: --rollup is the component (C3) view, --root scopes the call-graph render")
	}
	// The rollup is a WHOLE-SERVICE view: an --entry build prunes nodes/edges to the
	// entry cone and drops the unscoped disclosure sections, so a blind effect whose site
	// is out of cone would silently vanish from the component view — a hidden disclosure.
	// Refuse rather than emit a confidently-incomplete C3 map (fail closed).
	if entry != "" {
		return fmt.Errorf("graph --rollup and --entry are mutually exclusive: the component (C3) rollup is a whole-service view; an entry-scoped build would silently drop out-of-cone components and disclosed effects")
	}
	if diffBase != "" {
		base, err := loadGraphJSON(diffBase)
		if err != nil {
			return fmt.Errorf("--diff base graph: %w", err)
		}
		if asMermaid {
			_, err = os.Stdout.WriteString(render.Fence(graphio.RollupMermaidDiff(base, g, graphio.RollupMermaidOptions{Bands: bands})))
			return err
		}
		return emitCanonJSON(graphio.RollupDiff(base, g))
	}
	if asMermaid {
		_, err := os.Stdout.WriteString(render.Fence(g.RollupMermaid(graphio.RollupMermaidOptions{Bands: bands})))
		return err
	}
	return emitCanonJSON(g.RollupByPackage())
}

// emitCanonJSON writes v as canonical JSON to stdout — the same deterministic encoding
// the graph artifact uses, so a rollup is byte-identical across runs.
func emitCanonJSON(v any) error {
	b, err := canonjson.Marshal(v)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// loadGraphJSON decodes a committed graph JSON (a `flowmap graph` artifact) into a
// graphio.Graph for the --diff base side. It decodes STRICTLY (DisallowUnknownFields)
// so a base produced by a NEWER flowmap — carrying a field this build does not model —
// is REJECTED rather than silently decoded with that field dropped, which would
// produce a confidently-wrong delta. (The reverse skew, an older base missing a field,
// is surfaced by the diff's tool-mismatch caveat when both sides are stamped.) The
// full trust-boundary validation still lives in groundwork's own loader; this is the
// view path's forward-compatibility guard.
func loadGraphJSON(path string) (*graphio.Graph, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var g graphio.Graph
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("decode %s (a base from a newer flowmap, or not a graph JSON?): %w", path, err)
	}
	return &g, nil
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
	reclaimSQLFlag := fs.Bool("reclaim-sql", false, "apply the SQL const-accumulation label reclaimer before classifying (recovers verbs from constant-fragment SQL builders, shrinking the B2 opaque-SQL frontier)")
	reclaimTopicFlag := fs.Bool("reclaim-topic", false, "apply the bus const-topic label reclaimer before classifying (recovers PUBLISH/CONSUME targets from constant-set topics, shrinking the dynamic-bus frontier)")
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
	g, err := graphio.Build(res, "", reclaimOpts(*reclaimSQLFlag, *reclaimTopicFlag)...)
	if err != nil {
		return err
	}
	warnSkippedAnnotations(os.Stderr, g)
	if *reclaimFlag {
		graphio.ApplyReclaimers(g, res)
	}
	// The committed section keeps the unconfirmed routes as an aggregate count; the
	// on-demand view shows them per-route, so classify for the full result here.
	rep := frontier.Summarize(graphio.ClassifyFrontier(g), g.RouteEntrypointCount())
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

// cmdSchemaDrift cross-checks the DB tables a service's code writes against the
// tables its migrations define (docs/design/schema-drift-check-plan.md, §1). The
// code-side write set comes from the graph's boundary:db labels — read off an
// already-emitted --graph, or, when --graph is omitted, built fresh from [dir]
// (the one-step CI form). Either way it is a deterministic post-process over the
// graph; a code-side table absent from the defined schema is the
// `relation "X" does not exist` deploy-hazard class.
//
// Soundness: the drift verdict is sound iff the defined-schema set is COMPLETE, so
// the library-owned tables (outbox/inbox, auto-migrated by a library and named by no
// migration) must be declared (--library-owned or static.schemaCheck). The check
// inherits the db-call frontier — a write whose SQL is non-constant carries no table
// — so "no drift" means "no drift among resolved writes"; --reclaim-sql shrinks that
// opaque residual. Default is a disclosure (exit 0); --gate makes drift a non-zero
// exit for CI.
func cmdSchemaDrift(args []string) error {
	fs := flag.NewFlagSet("schema-drift", flag.ContinueOnError)
	graphPath := fs.String("graph", "", "path to an emitted graph JSON; omit to build the graph fresh from [dir]")
	migrationsDir := fs.String("migrations", "", "directory of SQL migrations (overrides static.schemaCheck.migrationsDir)")
	libraryOwned := fs.String("library-owned", "", "comma-separated tables a library auto-migrates (outbox/inbox), folded into the defined schema (overrides static.schemaCheck.libraryOwnedTables)")
	algo := fs.String("algo", "", `with build-fresh (no --graph): call-graph algorithm "rta" (default), "vta", "cha"`)
	reclaimSQL := fs.Bool("reclaim-sql", false, "with build-fresh: apply the SQL const-accumulation label reclaimer (recommended — shrinks the opaque-write frontier)")
	gate := fs.Bool("gate", false, "exit non-zero if any drift is found (CI gate)")
	asJSON := fs.Bool("json", false, "emit the report as canonical JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := dirArg(fs)
	if err != nil {
		return err
	}

	// The [dir]'s .flowmap.yaml supplies the schema source (migrations dir,
	// library-owned tables); flags override it. dirArg defaults dir to ".", so an
	// absent config just yields the zero value and the flags stand alone.
	cfg, err := config.LoadDir(dir)
	if err != nil {
		return err
	}
	migPath := *migrationsDir
	if migPath == "" && cfg.Static.SchemaCheck.MigrationsDir != "" {
		migPath = filepath.Join(dir, cfg.Static.SchemaCheck.MigrationsDir) // config path is relative to the service dir
	}
	if migPath == "" {
		return fmt.Errorf("schema-drift: no migrations source — pass --migrations <dir> or set static.schemaCheck.migrationsDir in %s", filepath.Join(dir, config.FileName))
	}
	libOwned := splitList(*libraryOwned)
	if len(libOwned) == 0 {
		libOwned = cfg.Static.SchemaCheck.LibraryOwnedTables
	}

	// Code-side edges: read an emitted graph (the build stays literally untouched),
	// or build fresh from [dir] when --graph is omitted.
	edges, source, err := schemaDriftEdges(*graphPath, dir, *algo, *reclaimSQL)
	if err != nil {
		return err
	}
	files, err := schemadrift.LoadMigrations(migPath)
	if err != nil {
		return err
	}

	rep := schemadrift.Check(edges, files, libOwned)
	if *asJSON {
		b, err := canonjson.Marshal(rep)
		if err != nil {
			return err
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return err
		}
	} else {
		fmt.Print(schemadrift.Render(source, rep))
	}
	// --gate turns the disclosure into a CI gate AFTER the report is emitted, so the
	// failure always ships the evidence with it.
	if *gate && len(rep.Drift) > 0 {
		return fmt.Errorf("schema-drift: %d table(s) written by code but absent from the defined schema (see report)", len(rep.Drift))
	}
	return nil
}

// schemaDriftEdges returns the code's boundary edges and a label for the report.
// With graphPath set it reads the emitted graph (the build is untouched); otherwise
// it builds the graph fresh from dir, applying the same algo/SQL-fold options the
// other static views use.
func schemaDriftEdges(graphPath, dir, algo string, reclaimSQL bool) ([]schemadrift.Edge, string, error) {
	if graphPath != "" {
		g, err := loadGraphJSON(graphPath)
		if err != nil {
			return nil, "", err
		}
		return graphEdges(g), graphPath, nil
	}
	opt, err := algoOption(algo)
	if err != nil {
		return nil, "", err
	}
	res, err := analyze.Analyze(dir, opt)
	if err != nil {
		return nil, "", err
	}
	g, err := graphio.Build(res, "", reclaimOpts(reclaimSQL, false)...) // schema-drift is DB-only; topic fold is irrelevant
	if err != nil {
		return nil, "", err
	}
	warnSkippedAnnotations(os.Stderr, g)
	return graphEdges(g), dir, nil
}

// graphEdges adapts the emitted graph's edges to the schemadrift Edge view (From/To
// only), keeping the schemadrift core decoupled from the graph builder (the same
// decoupling frontier.Input uses).
func graphEdges(g *graphio.Graph) []schemadrift.Edge {
	out := make([]schemadrift.Edge, len(g.Edges))
	for i, e := range g.Edges {
		out[i] = schemadrift.Edge{From: e.From, To: e.To}
	}
	return out
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

// reclaimOpts maps the --reclaim-sql / --reclaim-topic flags to build options. Each
// is its own knob (not folded into --reclaim) so a label-reclaimed verdict and an
// edge-reclaimed verdict stay separately auditable on the substrate line; the SQL and
// bus label folds are likewise kept distinct so each reclaimed target names its
// reclaimer (via=sqlfold.Via vs. via=topic-constfold).
func reclaimOpts(reclaimSQL, reclaimTopic bool) []graphio.BuildOption {
	var opts []graphio.BuildOption
	if reclaimSQL {
		opts = append(opts, graphio.WithSQLFold())
	}
	if reclaimTopic {
		opts = append(opts, graphio.WithTopicFold())
	}
	return opts
}

// warnSkippedAnnotations writes, to w (stderr at the CLI boundary), one warning per
// config annotation the build dropped because its (site, kind) named an
// algorithm-fragile blind-spot kind absent from THIS --algo's manifest at an
// otherwise-live site (§22). The skip itself happens in the pure graphio.Build (which
// records it on g.SkippedAnnotations, json:"-"); the WARNING is surfaced here so Build
// stays a byte-identical function of its inputs and stdout (the graph JSON / view) is
// untouched. A dropped annotation is disclosure-only — no count, edge, or verdict
// moves — so warn-and-skip is the right severity: louder than silent, never an exit-1
// on a note. The most likely fix is an --algo skew (the annotation was authored under
// vta; this build ran the CLI-default rta), so the message names the algo and the fix.
func warnSkippedAnnotations(w io.Writer, g *graphio.Graph) {
	for _, s := range g.SkippedAnnotations {
		_, _ = fmt.Fprintf(w,
			"flowmap: warning: skipped annotation at %s: no %q blind spot under --algo %s (present: %s); the %q kind is algorithm-dependent — keep --algo consistent with how the annotation was authored (e.g. --algo vta)\n",
			s.Site, s.Kind, g.Algo, strings.Join(s.Present, ", "), s.Kind)
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
	corpusDir := fs.String("corpus-dir", "", "write a stampless <flow>.<service>.golden.json impeach corpus per flow here (any mode); the form `groundwork verify --corpus` consumes")
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

	// The impeach corpus is also a producer side-effect, independent of the effects
	// gate (the gate over this corpus is `groundwork verify --corpus`, never here):
	// emit it in any mode when --corpus-dir is given.
	if *corpusDir != "" {
		if err := writeImpeachCorpus(*corpusDir, frags, spans); err != nil {
			return err
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
		if err := os.WriteFile(stem+".flow.md", []byte(render.Fence(render.Mermaid(fr.trace))), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s.effects.json (+ .flow.md)\n", stem)
	}
	return nil
}

// writeImpeachCorpus persists each canonicalized fragment as a stampless
// <flow>.<service>.golden.json through the SAME writer the in-test golden lifecycle
// uses (golden.WriteSnapshot), producing the committed-corpus form `groundwork verify
// --corpus` consumes — the wiring the design doc's "remaining: a flowmap CLI for the
// loop" called for. It is presence-only (impeach uses behavioral PRESENCE to impeach
// static ABSENCE, never the reverse), so a synthesized-root fragment — truncated, no
// clean inbound entry — is still sound to include (a partial trace can only OMIT a
// real effect, never invent one) and is written best-effort with a note, mirroring the
// cross-service view rather than the effects gate's skip. Same fail-closed collision
// guard as updateEffectGoldens: two fragments slugging to one stem are refused, never
// silently overwritten. The L0/L1 localization caveat is disclosed once, conditioned on
// whether the spans actually carry the in-process flowmap.fqn tag.
func writeImpeachCorpus(dir string, frags []ingestFragment, spans []capture.Span) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fmt.Println()
	claimed := map[string]string{} // file stem -> "flow / service" that wrote it
	for _, fr := range frags {
		stem := effectStem(fr.fc.Slug, fr.fc.Service)
		owner := fr.fc.Slug + " / " + fr.fc.Service
		if prev, ok := claimed[stem]; ok && prev != owner {
			return fmt.Errorf("corpus golden collision: %q and %q both map to %s.golden.json; rename a flow or service to disambiguate", prev, owner, stem)
		}
		claimed[stem] = owner
		if err := golden.WriteSnapshot(fr.trace, dir, stem); err != nil {
			return err
		}
		note := ""
		if fr.fc.Synthesized {
			note = " (synthetic root — no inbound entry span; presence still sound)"
		}
		fmt.Printf("wrote %s.golden.json (+ .flow.md)%s\n", filepath.Join(dir, stem), note)
	}
	if corpusHasFQNTags(spans) {
		fmt.Println("localization: spans carry flowmap.fqn — impeach severance localizes to the precise severed call site (L1)")
	} else {
		fmt.Println("localization: spans carry no flowmap.fqn tag (set in-process by the harness only), so impeach severance over this corpus is L0 (coarse: the entry/effect pair), not L1 (the precise severed call site) — still a sound impeach input; candidates, L0 severance, and the downgrade ladder all hold")
	}
	return nil
}

// corpusHasFQNTags reports whether any span carries the in-process flowmap.fqn L1
// localization tag (capture.FQNTagKey, set only by harness.OnStart). A from-collector
// export lacks it, so the impeach corpus localizes at L0; a harness-exported one keeps
// it and reaches L1. Detected from the spans, never assumed, so the caveat is honest.
func corpusHasFQNTags(spans []capture.Span) bool {
	for i := range spans {
		if spans[i].Attr(capture.FQNTagKey) != "" {
			return true
		}
	}
	return false
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
		if err := os.WriteFile(stem+".flow.md", []byte(render.Fence(md)), 0o644); err != nil {
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
		if c.Service == "" {
			// syscontext keys every overlay node/edge on the service name, so a
			// contract with no service (an unnamed module) can't be attributed and
			// addContract would drop its entire boundary surface. Disclose and skip
			// here — like a load failure — so the "+N overlaid" count stays honest
			// rather than counting a contract whose surface was silently omitted.
			fmt.Printf("  - contract %s: skipped: no service name (unnamed module); its boundary surface cannot be overlaid\n", d)
			continue
		}
		statics = append(statics, contractToSyscontext(c))
	}

	g := syscontext.Build(traces, statics, syscontext.Options{Choreography: choreography})
	path := filepath.Join(dir, "system.context.md")
	if err := os.WriteFile(path, []byte(render.Fence(render.SystemGraph(g))), 0o644); err != nil {
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
  graph [--entry R] [--algo A] [--mermaid] [--rollup package] [--reclaim] [--reclaim-sql] [--reclaim-topic] [dir]  print the non-gated call-graph view (--mermaid: flowchart; --rollup package: component/C3 view, with --diff a code-vs-disclosure delta; --reclaim* close sound seams/SQL/bus labels)
  frontier [--algo A] [--reclaim] [--reclaim-sql] [--reclaim-topic] [--json] [dir]  classify the static frontier (A/B/B2/C) — measurement, not a gate
  schema-drift [--graph G] [--migrations D] [--library-owned a,b] [--reclaim-sql] [--gate] [--json] [dir]  cross-check code DB writes against the migration-defined schema (omit --graph to build fresh from dir; dir's .flowmap.yaml supplies static.schemaCheck; --gate: non-zero exit on drift)
  diff <a.json> <b.json>     print the structural change set between two golden traces
  coverage [--flows D] [dir] boundary effects no committed flow exercises
  behavior ingest <traces>   map an OTLP/JSON trace export to boundary effects
                             [--service-dir D] coverage delta; [--flows-dir D] gate
                             on committed *.effects.json (--update to rebase)
  version                    print the flowmap version
  help                       show this message`)
}
