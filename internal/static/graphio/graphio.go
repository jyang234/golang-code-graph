// Package graphio renders the static pipeline's NON-gated view: the full
// first-party call graph with signatures and typed boundary edges (static-
// extractor spec §2, §9). Unlike the boundary contract, this view DOES include DB
// edges and internal call structure — it is the richer "what can happen" map for
// human understanding and the AI-assist surface. It is regenerated on demand and
// never gated, because function-level structure churns under refactoring.
package graphio

import (
	"errors"
	"fmt"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/boundarylabel"
	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	cg "github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/obligations"
	"github.com/jyang234/golang-code-graph/internal/static/rebind"
	"github.com/jyang234/golang-code-graph/internal/static/reclaim"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
	"github.com/jyang234/golang-code-graph/internal/static/signatures"
)

// Graph is the non-gated call-graph view, optionally scoped to one entry point.
// It carries the graph-completeness blind spots (reflect, high fan-out,
// unsafe/cgo/linkname) — disclosures that belong with the "what can happen" map
// rather than the gated boundary contract.
type Graph struct {
	// Stamp is an optional caller-supplied identity (typically the commit SHA
	// CI built from). It is an argument, never derived, so determinism holds:
	// the graph stays a pure function of its inputs, and goldens are generated
	// unstamped. groundwork's triage/mcp verify it via --expect.
	Stamp string `json:"stamp,omitempty"`

	// Tool is the flowmap build that PRODUCED this graph (buildinfo.Version of
	// the binary). Unlike Stamp — which is the caller-supplied identity of the
	// CODE — Tool is the identity of the PRODUCER, and the one provenance dimension
	// the consumer cannot supply: only the binary knows which build it is. It is
	// DERIVED (from the running binary), so it is set by the CLI layer, never by
	// Build — Build stays a pure function of its inputs, so the determinism test
	// and any golden built through Build are byte-identical regardless of which
	// flowmap built them. It travels as PROVENANCE beside Stamp/Algo so groundwork
	// can round-trip it and flag a base↔branch producer mismatch: "same code → same
	// graph" holds only WITHIN one tool version, and a pure tool-version bump can
	// otherwise surface as a phantom code delta (R11). Empty means unrecorded (a
	// pre-Tool flowmap, or a golden deliberately built tool-free), never "same tool".
	Tool       string `json:"tool,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`

	// Algo is the call-graph construction algorithm this graph was built on
	// (rta|vta|cha) and Caveats are its recorded soundness/precision notes. All
	// three are sound over-approximations modulo the reflection/unsafe frontier
	// already disclosed as blind spots — VTA is RTA-seeded and refines dynamic
	// dispatch by type-flow without dropping real edges, so it is a blessed proof
	// substrate, not exploration-only. These travel in the graph JSON as
	// PROVENANCE: groundwork's fitness/review/verify echo them so a gated verdict
	// self-certifies which substrate it was computed on. The callgraph package
	// computes both; this is where they cross into the emitted interface.
	Algo       string                 `json:"algo,omitempty"`
	Caveats    []string               `json:"caveats,omitempty"`
	Nodes      []Node                 `json:"nodes"`
	Edges      []Edge                 `json:"edges"`
	BlindSpots []blindspots.BlindSpot `json:"blind_spots"`

	// Obligations is the path-obligation disclosure section: per-site verdicts
	// for the .flowmap.yaml obligation rules, FQN-keyed and separate from the
	// call-graph edges (a narrow level-2 slice). Omitted entirely when no rules
	// are configured, so rule-free services emit byte-identical graphs.
	Obligations []obligations.Finding `json:"obligations,omitempty"`

	// Entrypoints maps each named root (HTTP route, bus topic) to its handler
	// function — the route→fn join neither artifact carried before. The names
	// are REGISTRATION-SITE literals: a stdlib HandleFunc root has no method,
	// and a route mounted under a router prefix carries only its leaf pattern
	// — which is why the triage resolver matches them segment-wise rather than
	// exactly. The fn is the registered handler ARGUMENT, so a middleware
	// wrapper resolves to the wrapping closure (one hop upstream of the human
	// expectation; its forward reach still covers the real handler). Like the
	// other level-2 slices it rides unscoped builds only.
	Entrypoints []Entrypoint `json:"entrypoints,omitempty"`

	// CompositionRoots are the import paths of this unit's COMPOSITION-ROOT
	// packages — the `package main` commands, taken from the authoritative root set
	// (roots.KindMain / ssautil.MainPackages), sorted and deduped. It is the
	// trustworthy answer to "which package assembles the program": a `package main`
	// cannot be imported, so no first-party package can make a named call into it —
	// every graph edge whose TARGET is a composition root is therefore a
	// dependency-injected func value (a closure or method value main passed into a
	// component and that component later invokes), never a domain dependency. The C3
	// rollup reads this to mark the composition-root component and reclassify those
	// back-edges as wiring. Derived from SSA, NOT from an FQN string heuristic, so a
	// non-main package that merely declares a package-level `func main` (legal Go, a
	// smell) is never mistaken for one. Disclosure-only — same trust class as
	// Node.Package: no verdict, count, edge, tier, or reachability computation reads
	// it. Omitted when the unit builds no command (a library), so those graphs stay
	// byte-identical. A whole-program fact like Entrypoints: it rides unscoped builds
	// only (the rollup that consumes it refuses an --entry scope).
	CompositionRoots []string `json:"composition_roots,omitempty"`

	// OmittedPackages are the first-party packages a rendered C3 component IMPORTS
	// but that declare NO functions (types/consts only) — so they contribute no
	// call-graph node and are absent from the component rollup, with no signal a
	// reader orienting on the architecture would otherwise see they exist. The C3
	// rollup discloses them as a footnote (tenet 3: say where the map is blind
	// explicitly), so a real internal domain package — imported by the command and
	// the api/app layers but types-only — is not silently dropped from the
	// orientation view. Confined to first-party imports OF a component: an absent
	// stdlib/third-party dep is expected, and a first-party package no component
	// imports is genuinely unreferenced (e.g. a co-located client SDK the service
	// binary does not link), so neither is listed — only an imported-but-invisible
	// internal package is. "No functions" is a STRUCTURAL fact (collected through the
	// complete ssautil.AllFunctions walk, so a methods-only package is not miscounted
	// as function-less), independent of the call-graph algo — distinct from a package
	// whose functions are merely UNREACHABLE (that is the frontier/missed-root
	// concern, not this one), which is why a has-functions package never appears here.
	// Disclosure-only — same trust class as CompositionRoots: no verdict, count,
	// edge, tier, or reachability computation reads it. A whole-program fact like
	// Entrypoints: it rides unscoped builds only (a scoped --entry view prunes
	// components, so the imported-by-a-component set would be a scoping artifact).
	// Sorted, deduped, and omitted when empty, so a service with no types-only
	// internal package emits a byte-identical graph.
	OmittedPackages []string `json:"omitted_packages,omitempty"`

	// EffectOrder is the partial-effect disclosure (incident-triage plan IT-3):
	// for each function holding both a committed external effect (bus publish,
	// DB mutation) and a fallible call, whether the effect can — or always
	// does — execute before that call. Triage reads it to answer "if this call
	// faults, what may already be committed?". Like Obligations it rides
	// unscoped builds only and is omitted when empty.
	EffectOrder []obligations.EffectOrder `json:"effect_order,omitempty"`

	// Frontier is the A/B/B2/C classification of where static reachability stops
	// being able to answer (docs/design/frontier-instrumentation-plan.md): the
	// dynamic effects, the strict-server dispatch seams, the opaque-SQL writes, the
	// over-approximated dispatch — plus the AGGREGATE unconfirmed-route count and the
	// coverage caveat, so a consumer reading only this section cannot misread a 0
	// attribution loss as a proof of no severance. A read-only disclosure (it changes
	// no verdict — R3), omitted when there is nothing to disclose so a clean,
	// unscoped service emits a byte-identical graph.
	Frontier *FrontierSection `json:"frontier,omitempty"`

	// Annotations are human/AI CONTEXT attached to blind spots (config
	// static.annotations), keyed by (Site, Kind) to the manifest above. A
	// disclosure-only channel: no verdict, count, or reachability computation reads
	// it; it explains a blind spot the machine cannot close, it never closes one.
	// Each is matched to a detected blind spot at build time. An unmatched one fails
	// the build (drift, not a silent drop) EXCEPT the one tolerable case: an
	// annotation naming an algorithm-fragile kind (blindspots.AlgoFragile) absent
	// from this build's --algo manifest at an otherwise-live site is warn-and-skipped
	// into SkippedAnnotations instead (§22), since a hard build failure on a skewed
	// disclosure-only note is worse than the lost note. Omitted when none, so an
	// annotation-free service emits a byte-identical graph.
	Annotations []Annotation `json:"annotations,omitempty"`

	// SkippedAnnotations records config annotations the merge DROPPED because their
	// (Site, Kind) named an algorithm-fragile blind-spot kind absent from THIS build's
	// manifest while the Site stayed live (an --algo/flag skew, not a stale FQN — §22).
	// It is in-memory only (json:"-"): it never serializes, so no golden churns and the
	// graph stays byte-identical per --algo, but the CLI boundary reads it to warn the
	// operator that a disclosure-only note was skipped rather than the build hard-failing.
	// Exported (not unexported like foldSQL) precisely so that cross-package CLI surface
	// can read it. Disclosure-only by construction — a dropped annotation changes no
	// count, edge, or verdict.
	SkippedAnnotations []SkippedAnnotation `json:"-"`

	// foldSQL records whether this graph was built with the SQL const-fold
	// (--reclaim-sql / WithSQLFold). Unexported, so it never serializes (no golden
	// churn) and is an in-memory build flag only: the frontier classifier reads it
	// to split the opaque-db disclosure into B2a/B2b, taken from the flag rather than
	// inferred from tagged edges so an all-abstain fold run still reads as folded.
	foldSQL bool

	// reclaimEdges is the in-graph dispatch-seam reclaim edge set (both endpoints are
	// nodes), computed ONCE at build as a dry run of reclaim.StrictServer. It is the
	// single source both the frontier dry run (which bins a severed closure B only if
	// its reconnect edge is here) and ApplyReclaimers (which folds these edges in) read,
	// so the "is this seam reclaimable" answer cannot differ between the prediction and
	// the apply, and StrictServer runs once rather than once per consumer (§21.②).
	// Unexported, so it never serializes (no golden churn). Populated only on an unscoped
	// build (the frontier rides those); a scoped ApplyReclaimers recomputes it.
	reclaimEdges []reclaim.Edge

	// middlewareReclaim is the dry run of reclaim.MiddlewareChain, computed ONCE at an
	// unscoped build. Like reclaimEdges it is the single source the frontier dry run reads
	// (a standalone middleware UnresolvedCall whose loop the reclaimer proves EMPTY is binned
	// B — reclaimable by --reclaim-middleware — instead of A) and ApplyMiddlewareReclaimer
	// folds, so prediction and apply cannot diverge. Unexported (never serializes); a scoped
	// ApplyMiddlewareReclaimer recomputes it.
	middlewareReclaim reclaim.MiddlewareResult
}

// nodeSet returns the set of node FQNs in g — the membership map both the reclaim
// dry run and ApplyReclaimers filter against. One builder so they cannot disagree.
func (g *Graph) nodeSet() map[string]bool {
	nodes := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		nodes[n.FQN] = true
	}
	return nodes
}

// nodePackageSet returns the set of packages that contributed a node to g — the
// "rendered component" set. It is the ONE predicate for "this package is visible in
// the rollup" (a non-empty Node.Package), so the omitted-package anchor reads the same
// notion of a component the rollup renders (RollupByPackage's per-package node counts
// have these exact keys). nodeSet keys on FQN; this keys on Package — the C3 altitude.
func (g *Graph) nodePackageSet() map[string]bool {
	pkgs := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.Package != "" {
			pkgs[n.Package] = true
		}
	}
	return pkgs
}

// FrontierSection is the disclosed frontier carried in the graph: the per-site
// markers, plus an AGGREGATE count of routes whose severance could not be confirmed
// (the third state — kept a count, not per-route markers, so it stays stable under
// refactoring and does not cry wolf on every health endpoint), plus the coverage
// caveat naming what the attribution signal confirms. Per-route detail for the
// unconfirmed routes lives in the on-demand `flowmap frontier` view, not here.
type FrontierSection struct {
	Markers           []frontier.Marker `json:"markers,omitempty"`
	UnconfirmedRoutes int               `json:"unconfirmed_routes,omitempty"`
	Coverage          string            `json:"coverage,omitempty"`
}

// Entrypoint is one named root: a discovered HTTP route or consumed topic, or a
// DECLARED callback/worker (config.entrypoints), with the function that handles it.
type Entrypoint struct {
	// Kind is "http" or "consumer" for a discovered route, or "callback"/"worker"
	// for an author-declared root call-resolution could not reach — the kind is the
	// provenance: declared roots are asserted, not discovered.
	Kind string `json:"kind"`
	// Name is the route/topic for a discovered root ("POST /loan-application",
	// "payment.settled"); for a declared root it is the "import/path#Symbol"
	// reference it was asserted from (no route or event name is statically known).
	Name string `json:"name"`
	Fn   string `json:"fn"`
}

// Node is one first-party function.
type Node struct {
	FQN  string `json:"fqn"`
	Sig  string `json:"sig"`
	Tier int    `json:"tier"`
	// Package is the node's defining Go import path — the typed package fact a
	// consumer would otherwise have to recover by string-splitting FQN (a display
	// string: paren-wrapped receivers, "$1" closure suffixes, generics, promoted
	// methods), where the first-party/test/external distinction is a typing fact a
	// parse cannot reliably reconstruct. Derived via features.EffectivePkgPath, which
	// resolves the nil-fn.Pkg synthetics go/ssa produces — a generic INSTANCE through
	// its Origin, a $bound/$thunk wrapper through its wrapped method Object — so a
	// rendered instance/wrapper node still names its real package (C-1). Disclosure-
	// only — same trust class as BlindSpot.Package: no verdict, count, edge, tier, or
	// reachability computation reads it, so it cannot move a pole. Empty only for a
	// truly package-less synthetic (none resolves); omitempty spares exactly those.
	Package  string `json:"package,omitempty"`
	Fallible bool   `json:"fallible,omitempty"`
	// File / Line / EndLine locate the node's declaration in source: File is the
	// defining file made RELATIVE to the service dir (so it is byte-identical across
	// checkouts, the determinism discipline obligations.site already uses), falling
	// back to the portable "<import-path>/<base>" form for a file outside the dir;
	// Line and EndLine are the 1-based span of the `func` declaration (keyword through
	// closing brace) from the AST node, never invented. They let a caller intersect a
	// git diff's changed line ranges with each node's span to recover the author-edited
	// FQN set `review-triage --scope-fqns` consumes — the one signal not derivable from
	// the graph's FQN/package/sig/tier alone — and unlock other position-aware views
	// (jump-to-source, blast-radius by file). Pure function of the SSA function and the
	// FileSet, and disclosure-only: same trust class as Package — no verdict, count,
	// edge, tier, or reachability computation reads them, so they cannot move a pole.
	// All three are empty/zero together, and omitempty away, for a synthetic node with
	// no syntax (a compiler-generated wrapper or bound method) or an invalid position —
	// disclosure that fails quiet, never a fabricated line.
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
}

// nodePosition returns fn's defining file and 1-based declaration span for the
// disclosure-only File/Line/EndLine node fields. The span comes from the AST node
// (fn.Syntax(): the *ast.FuncDecl or *ast.FuncLit) — the `func` keyword through the
// closing brace — so a caller intersecting a diff sees the function BODY, not its name
// wherever else it is mentioned (a call site in an edited caller, an FQN string in a
// committed golden), which is exactly the distinction a grep or hunk-header extractor
// gets wrong. The file path is made service-relative — hence byte-identical across
// checkouts — through features.RelFile, the SAME predicate obligations.site uses (one
// source of truth). A synthetic function (nil Syntax) or an invalid start position
// yields ("", 0, 0): all three fields omitempty away rather than carrying a fabricated
// line. Pure function of the SSA function and the FileSet.
func nodePosition(fn *ssa.Function, baseDir string) (file string, start, end int) {
	syn := fn.Syntax()
	if syn == nil {
		return "", 0, 0
	}
	fset := fn.Prog.Fset
	sp := fset.Position(syn.Pos())
	if !sp.IsValid() {
		return "", 0, 0
	}
	end = sp.Line
	if ep := fset.Position(syn.End()); ep.IsValid() {
		end = ep.Line
	}
	return features.RelFile(sp.Filename, baseDir, features.PkgPath(fn)), sp.Line, end
}

// Edge is a call from a first-party function to another first-party function or
// to a typed boundary node (DB, external service, or bus).
type Edge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Tier       int    `json:"tier"`
	Boundary   string `json:"boundary,omitempty"`
	Concurrent bool   `json:"concurrent,omitempty"`

	// Via names the reclaimer that recovered this edge, empty for a base
	// call-graph edge (Phase 3 / D2). A reclaimed edge is one real execution can
	// take that the builder lost at a dispatch seam; carrying its provenance lets a
	// reviewer diff base-vs-reclaimed and a verdict self-certify which reclaimers
	// it leaned on.
	Via string `json:"via,omitempty"`
}

// mergeDeclaredBlindSpots appends the config's human-ratified seams (§8 enactment)
// to the auto-detected graph blind spots, deterministically — sorted through the one
// canonical comparator (blindspots.SortBlindSpots), so the result is byte-identical
// regardless of declaration order. A declared seam makes static abstain at its site
// (the safe direction: it can only weaken proofs, never hide a violation). The
// default kind is ImpeachmentSeam (the behaviorally-discovered category); an explicit
// kind is kept verbatim but MUST name a recognized blindspots.Kind — an unknown kind
// is a config error (returned), never a silent passthrough that would let a typo'd
// kind ride the gated artifact. An entry with no site is skipped (nothing to blind;
// config.validate already rejects this on load, so the skip is belt-and-suspenders
// for callers that build a Config directly).
//
// The DETECTED spots pass through VERBATIM — they are already full-struct-deduped by
// blindspots.Detect, and two detected spots that share (kind, site) but differ in
// Detail are DISTINCT disclosures (e.g. two over-threshold dispatch sites in one
// function, each with its own callee count), so collapsing them would silently drop a
// disclosed blind spot — the fail-OPEN direction. Only the DECLARED seams are
// collapsed: among themselves by (kind, site) keeping the lexically-smallest Detail (an
// intrinsic tie-break, never arrival order, per CLAUDE.md determinism), and a declared
// seam whose (kind, site) is already detected is dropped as redundant (the detected
// disclosure already forces abstention there, and it is the authoritative text).
func mergeDeclaredBlindSpots(detected []blindspots.BlindSpot, cfg *config.Config) ([]blindspots.BlindSpot, error) {
	if cfg == nil || len(cfg.Static.DeclaredBlindSpots) == 0 {
		return detected, nil
	}
	detectedKeys := map[[2]string]bool{}
	for _, b := range detected {
		detectedKeys[[2]string{string(b.Kind), b.Site}] = true
	}
	// Collapse DECLARED seams among themselves, keeping the lexically-smallest Detail.
	declaredByKey := map[[2]string]blindspots.BlindSpot{}
	for i, d := range cfg.Static.DeclaredBlindSpots {
		kind := d.Kind
		if kind == "" {
			kind = string(blindspots.ImpeachmentSeam)
		}
		if !blindspots.Recognized(blindspots.Kind(kind)) {
			return nil, fmt.Errorf("flowmap config: static.declaredBlindSpots[%d] (%s): kind %q is not a recognized blind-spot category", i, d.Site, kind)
		}
		key := [2]string{kind, d.Site}
		if d.Site == "" || detectedKeys[key] {
			continue // nothing to blind, or already a detected disclosure (detected wins)
		}
		cand := blindspots.BlindSpot{Kind: blindspots.Kind(kind), Site: d.Site, Detail: d.Reason}
		if cur, ok := declaredByKey[key]; !ok || cand.Detail < cur.Detail {
			declaredByKey[key] = cand
		}
	}
	out := append([]blindspots.BlindSpot(nil), detected...)
	for _, b := range declaredByKey {
		out = append(out, b)
	}
	blindspots.SortBlindSpots(out)
	return out, nil
}

// Annotation is human/AI context on a blind spot, keyed by (Site, Kind) to the
// graph's blind-spot manifest. Disclosure only — no consumer verdict reads it.
type Annotation struct {
	Site  string `json:"site"`
	Kind  string `json:"kind"`
	Note  string `json:"note"`
	By    string `json:"by,omitempty"`
	Claim string `json:"claim,omitempty"`
}

// SkippedAnnotation records one config annotation the merge dropped because its
// (Site, Kind) named an algorithm-fragile blind-spot kind (blindspots.AlgoFragile)
// absent from THIS build's manifest, while the Site stayed live — it still carries
// the Present kinds, so this is an --algo/flag skew, not a stale FQN (§22). It is
// disclosure context for the build operator, never a verdict input.
type SkippedAnnotation struct {
	Site    string
	Kind    string
	Present []string
}

// annotationLess is the total intrinsic order used to break dedup ties on a
// colliding (site, kind): compare Note, then By, then Claim. Totality matters —
// falling back to arrival order on equal notes would make the kept by/claim depend
// on config-file position, not content.
func annotationLess(a, b Annotation) bool {
	if a.Note != b.Note {
		return a.Note < b.Note
	}
	if a.By != b.By {
		return a.By < b.By
	}
	return a.Claim < b.Claim
}

// mergeAnnotations matches each config annotation to a blind spot already in the
// manifest and returns the bound annotations, sorted by (Site, Kind) for a
// byte-identical result. An annotation is CONTEXT on a detected gap, so it must
// name one: an annotation that matches no blind spot is refused (a stale FQN or a
// kind that no longer fires is drift, the fail-closed direction — never a silent
// drop). When kind is omitted it binds a site that carries exactly one blind spot;
// a site with several requires the kind, so context can never attach to the wrong
// shape. The match is against the graph's blind-spot subset (the manifest a
// consumer sees) — the gated boundary disclosures live in their own channel.
//
// The ONE tolerated mismatch (§22): when the requested kind is absent at an
// otherwise-LIVE site (config.KindAbsentError, so the FQN still resolves to a
// detected blind spot — it is not a stale-FQN orphan) AND that kind is
// algorithm-fragile (blindspots.AlgoFragile — its presence flips with --algo, e.g.
// an UnresolvedCall that fires under vta but not the CLI-default rta), the
// disclosure-only annotation is WARN-AND-SKIPPED into the returned skip list rather
// than failing the build. A hard build failure on an --algo/flag skew is worse than
// the lost note, and an annotation never moves a count or a verdict. Every other
// error — an orphan site, an ambiguous multi-kind site, or a mismatch on an
// algo-STABLE kind (a real typo) — still fails closed.
//
// Known limit of the relaxation: it requires the site to stay LIVE (carry another
// blind spot) under the dropping algo. A site whose ONLY blind spot was the fragile
// kind has zero blind spots once the algo drops it, which is byte-for-byte
// indistinguishable from a stale FQN — so it takes the orphan path and FAILS the
// build, even though the cause is an --algo skew, not a typo. That is the sound
// direction (fail closed): the alternative — skipping any fragile-kind annotation at
// a zero-blind-spot site — would silently swallow a genuine typo. The fix for such a
// site is to pin --algo (build under the algo that surfaces the kind), not to relax
// here. See docs/specs/static-extractor-spec.md §7 (annotations).
func mergeAnnotations(manifest []blindspots.BlindSpot, cfg *config.Config) ([]Annotation, []SkippedAnnotation, error) {
	if cfg == nil || len(cfg.Static.Annotations) == 0 {
		return nil, nil, nil
	}
	// Index the manifest: the kinds present at each site (deduped by the resolver).
	kindsAt := map[string][]string{}
	for _, b := range manifest {
		kindsAt[b.Site] = append(kindsAt[b.Site], string(b.Kind))
	}
	byKey := map[[2]string]Annotation{}
	var skipped []SkippedAnnotation
	for i, a := range cfg.Static.Annotations {
		// The binding rule lives in config.ResolveAnnotationKind so the producer here
		// and the read-only MCP `annotate` proposer cannot drift (parity test below).
		kind, err := config.ResolveAnnotationKind(a.Site, a.Kind, kindsAt[a.Site])
		if err != nil {
			// A live site whose requested kind is merely absent under THIS --algo, and
			// the kind is algorithm-fragile: skip the disclosure-only note (recorded for
			// the CLI to warn), do not abort the build. AlgoFragile is consulted on the
			// REQUESTED kind (a.Kind), the one that was not found — not on what is present.
			var ka *config.KindAbsentError
			if errors.As(err, &ka) && blindspots.AlgoFragile(blindspots.Kind(ka.RequestedKind)) {
				skipped = append(skipped, SkippedAnnotation{
					Site:    ka.Site,
					Kind:    ka.RequestedKind,
					Present: append([]string(nil), ka.Present...),
				})
				continue
			}
			return nil, nil, fmt.Errorf("flowmap config: static.annotations[%d]: %w", i, err)
		}
		// Collapse duplicate (site, kind) annotations to the lexically-smallest
		// (Note, By, Claim) — a TOTAL intrinsic tie-break, never arrival order: two
		// entries with the same note but different by/claim still resolve on content,
		// not config-file position (CLAUDE.md determinism).
		cand := Annotation{Site: a.Site, Kind: kind, Note: a.Note, By: a.By, Claim: a.Claim}
		key := [2]string{a.Site, kind}
		if cur, ok := byKey[key]; !ok || annotationLess(cand, cur) {
			byKey[key] = cand
		}
	}
	out := make([]Annotation, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		return out[i].Kind < out[j].Kind
	})
	// Sort the skip list on the same intrinsic (Site, Kind) key so the CLI warning
	// order — and any test over it — is byte-identical across runs.
	sort.Slice(skipped, func(i, j int) bool {
		if skipped[i].Site != skipped[j].Site {
			return skipped[i].Site < skipped[j].Site
		}
		return skipped[i].Kind < skipped[j].Kind
	})
	return out, skipped, nil
}

// Build renders the full first-party graph of res. If entry is non-empty, the
// graph is scoped to the functions reachable from the matching entry-point root.
// BuildOption tunes a graph build. Options are opt-in refinements (D2-style): the
// zero set reproduces the default graph byte-for-byte, so every committed golden
// is unchanged unless a caller explicitly asks for a refinement.
type BuildOption func(*buildOptions)

type buildOptions struct {
	foldSQL bool
	foldBus bool
}

// WithSQLFold enables the SQL const-accumulation label reclaimer (--reclaim-sql):
// DB effects whose statement is non-constant at the call site but provably built
// from constant fragments get their verb recovered and the edge tagged
// via=sqlfold.Via. Off by default; sound-or-abstain (docs/design/
// sql-constfold-reclaim-plan.md).
func WithSQLFold() BuildOption { return func(o *buildOptions) { o.foldSQL = true } }

// WithTopicFold enables the bus reclaim-topic label reclaimer (--reclaim-topic): a
// PUBLISH/CONSUME whose topic is non-constant at the call site but resolves to a
// finite, provably-complete set of constants gets that set named (one edge per topic,
// tagged via=viaTopicFold) instead of <dynamic>. The reclaim-sql analog for bus
// targets; off by default, sound-or-abstain, and verdict-neutral (the topic is only a
// target name).
func WithTopicFold() BuildOption { return func(o *buildOptions) { o.foldBus = true } }

func Build(res *analyze.Result, entry string, opts ...BuildOption) (*Graph, error) {
	var o buildOptions
	for _, opt := range opts {
		opt(&o)
	}
	ext := features.NewExtractor(res.Config, res.Program.ModulePath)
	hints := ext.Hints()

	scope := firstPartyScope(res)
	if entry != "" {
		root, err := resolveEntryRoot(res, entry)
		if err != nil {
			return nil, err
		}
		if root == nil {
			return nil, &EntryNotFoundError{Entry: entry}
		}
		scope = reachableFirstParty(res, root)
	}

	// The reverse call-graph index, only under --reclaim-sql: the Tier-B/C
	// re-attribution enumerates a pass-through helper's callers from it. Nil
	// otherwise, so the default build pays nothing.
	var callers map[*ssa.Function][]ssa.CallInstruction
	if o.foldSQL {
		callers = callerIndex(res)
	}

	g := &Graph{
		Entrypoint: entry,
		Algo:       string(res.Graph.Algo),
		Caveats:    res.Graph.Caveats,
		Nodes:      []Node{}, Edges: []Edge{}, BlindSpots: []blindspots.BlindSpot{},
		foldSQL: o.foldSQL,
	}
	if gs := blindspots.Graph(blindspots.Detect(res, hints)); len(gs) > 0 {
		g.BlindSpots = gs
	}
	// Merge human-ratified seams declared in config (the behavioral-impeachment
	// loop's enactment, §8): sites where static must abstain because behavior proved
	// the disclosure incomplete. Done here so a declared seam rides the graph exactly
	// like an auto-detected blind spot — the consumer (groundwork) cannot tell them
	// apart, and the next run is honest at the seam.
	merged, err := mergeDeclaredBlindSpots(g.BlindSpots, res.Config)
	if err != nil {
		return nil, err
	}
	g.BlindSpots = merged
	// Bind human/AI context to the finalized manifest (detected + declared). An
	// annotation that matches no blind spot fails the build — a disclosure-only
	// channel still fails closed on drift, never attaching context to a vanished site
	// — EXCEPT an algorithm-fragile kind absent at a live site, which is skipped (not
	// failed) and recorded for the CLI to warn on (§22). Build stays pure: the skip
	// list rides the in-memory Graph (json:"-"), so a given --algo regenerates
	// byte-identically; the warning is emitted at the CLI boundary, not here.
	annots, skipped, err := mergeAnnotations(g.BlindSpots, res.Config)
	if err != nil {
		return nil, err
	}
	g.Annotations = annots
	g.SkippedAnnotations = skipped
	rootFns := rootFuncSet(res)
	if entry == "" {
		for _, r := range res.Roots.Roots {
			if r.Name == "" || !scope[r.Func] {
				continue
			}
			switch r.Kind {
			case roots.KindHTTP, roots.KindConsumer, roots.KindCallback, roots.KindWorker:
				// "callback"/"worker" are DECLARED roots: the kind itself carries the
				// provenance (author-vouched, not discovered), and their Name is the
				// config reference they were asserted from, so a reader can tell a
				// declared entry from a discovered route.
				g.Entrypoints = append(g.Entrypoints, Entrypoint{Kind: string(r.Kind), Name: r.Name, Fn: r.FQN()})
			}
		}
		sort.Slice(g.Entrypoints, func(i, j int) bool {
			a, b := g.Entrypoints[i], g.Entrypoints[j]
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Fn < b.Fn
		})
		g.CompositionRoots = compositionRoots(res)
	}
	// The service dir, absolute — the anchor that makes both the obligations site
	// strings (entry=="" only) and the per-node File paths (every build) relative,
	// hence byte-identical across checkouts. Computed unconditionally: an entry-scoped
	// build never reaches the obligations section, but its nodes still carry positions.
	abs, err := filepath.Abs(res.Dir)
	if err != nil {
		return nil, err
	}
	base := abs

	// Lazily-shared summary engine (CX-2/CX-3): obligations and derived
	// effect sites consult the same instance, and rule-free, effect-free
	// services never construct it.
	var sums *obligations.Summaries
	summaries := func() *obligations.Summaries {
		if sums == nil {
			sums = obligationSummaries(res)
		}
		return sums
	}

	directEffects := map[*ssa.Function][]obligations.EffectSite{}
	labelSites := map[string]map[ssa.Instruction]bool{}
	for _, n := range res.Graph.Nodes {
		fn := n.Func
		if !scope[fn] {
			continue
		}
		// The node's outgoing edges are computed first because they decide the
		// node's tier: a function is as salient as the most consequential boundary
		// it directly reaches. Committed-effect sites (publish, DB mutation) are
		// collected in the same pass — this is the one place where the boundary
		// label and the ssa call site coexist (IT-3 scoping note).
		var nodeEdges []Edge
		for _, e := range n.Out {
			edges := edgeOf(ext, hints, e, scope, o.foldSQL, o.foldBus, callers, 0)
			nodeEdges = append(nodeEdges, edges...)
			// Record a committed effect only when the site yields exactly ONE — the
			// unambiguous case. A fold-resolved finite-table write fans the site out
			// into one edge PER possible target (e.g. DELETE publishers + DELETE
			// subscribers), but only one of them is committed on any given run: each is
			// a SOME-paths effect, so recording it as a directEffect would let the
			// proof-only effect-order/fault-card derivation (below) cite a target the
			// execution never wrote. The fan-out still rides g.Edges for the
			// write-surface budget; it just must not enter the always-effect channel.
			// edges[0].From == fn excludes a §19-re-attributed effect, whose From is a
			// CALLER, not the helper node fn being processed: its committed-effect /
			// always-effect derivation belongs to the caller (where the statement is
			// built), not to the helper sink, so it must not enter fn's effect channel.
			if entry == "" && e.Site != nil && len(edges) == 1 && edges[0].From == fn.RelString(nil) && committedEffect(edges[0].To) {
				directEffects[fn] = append(directEffects[fn], obligations.EffectSite{Label: edges[0].To, Site: e.Site})
				if labelSites[edges[0].To] == nil {
					labelSites[edges[0].To] = map[ssa.Instruction]bool{}
				}
				labelSites[edges[0].To][e.Site] = true
			}
		}
		file, line, endLine := nodePosition(fn, base)
		g.Nodes = append(g.Nodes, Node{
			FQN:      fn.RelString(nil),
			Sig:      signatures.Of(fn),
			Tier:     nodeTier(ext, fn, rootFns[fn], nodeEdges),
			Package:  features.EffectivePkgPath(fn),
			Fallible: fallible(fn),
			File:     file,
			Line:     line,
			EndLine:  endLine,
		})
		g.Edges = append(g.Edges, nodeEdges...)
	}

	// Effect-order pass (IT-3, extended by CX-3). It runs after every label's
	// site set is complete: a call to a first-party callee that performs a
	// labeled effect on EVERY path (an ALWAYS-effect summary) is a derived
	// effect site at the call instruction, carrying the callee in `via`. The
	// derivation is proof-only — a some-paths effect derives nothing, so a
	// fault card never cites an effect that might not have happened.
	if entry == "" && len(labelSites) > 0 {
		labels := make([]string, 0, len(labelSites))
		for l := range labelSites {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		for _, n := range res.Graph.Nodes {
			fn := n.Func
			if !scope[fn] {
				continue
			}
			sites := directEffects[fn]
			for _, e := range n.Out {
				callee := e.Callee.Func
				if e.Site == nil || !scope[callee] {
					continue
				}
				for _, l := range labels {
					if summaries().AlwaysEffect(callee, l, labelSites[l]) {
						sites = append(sites, obligations.EffectSite{Label: l, Site: e.Site, Via: callee.RelString(nil)})
					}
				}
			}
			g.EffectOrder = append(g.EffectOrder, obligations.OrderFacts(fn, sites, base)...)
		}
	}

	// Obligations are a whole-service disclosure (a level-2 slice of the FULL
	// graph). An entry-scoped view evaluates only the entry's cone, where a
	// rule anchored elsewhere would read UNMATCHED ("inert") and out-of-cone
	// verdicts would vanish — scoping artifacts presented as rule deadness. So
	// the section rides unscoped builds only.
	if rules := res.Config.Obligations; len(rules) > 0 && entry == "" {
		var fns []*ssa.Function
		for _, n := range res.Graph.Nodes {
			if scope[n.Func] {
				fns = append(fns, n.Func)
			}
		}
		g.Obligations = obligations.Check(rules, fns, base, summaries())
	}

	sortGraph(g)
	// Classify the frontier over the finalized graph — a read-only disclosure
	// section, computed last so it sees every node, edge, blind spot, and entry.
	// Like Obligations/EffectOrder it is a whole-service disclosure: a scoped
	// (--entry) cone drops entrypoints and prunes effect paths, so its starvation /
	// attribution-loss signal would be a scoping artifact, not a finding. Gate it on
	// the unscoped build, the same convention those sections use.
	if entry == "" {
		g.OmittedPackages = omittedPackages(res, g)
		g.reclaimEdges = reclaimEdges(res, g.nodeSet())
		g.middlewareReclaim = reclaim.MiddlewareChain(res)
		g.Frontier = frontierSection(g)
	}
	return g, nil
}

// reclaimEdges returns every sound reclaimer's edges whose BOTH endpoints are graph
// nodes — computed as a DRY RUN over res WITHOUT folding the edges, so even the default
// (un-reclaimed) frontier knows which severed closures are genuinely reclaimable. It is
// the SINGLE in-graph reclaim-edge predicate (CLAUDE.md: one source of truth): the
// frontier dry run derives its reclaimable-closure set from `.To` here, and
// ApplyReclaimers folds these same edges, so the "is this seam reclaimable" answer
// cannot differ between the prediction and the apply (§21.②). Each reclaimer's iteration
// order is deterministic and the cross-reclaimer concatenation order is fixed, so the
// result is too; deduped on (From, To) across all reclaimers.
func reclaimEdges(res *analyze.Result, nodes map[string]bool) []reclaim.Edge {
	seen := map[[2]string]bool{}
	var out []reclaim.Edge
	add := func(edges []reclaim.Edge) {
		for _, e := range edges {
			if !nodes[e.From] || !nodes[e.To] {
				continue
			}
			key := [2]string{e.From, e.To}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, e)
		}
	}
	add(reclaim.StrictServer(res))
	add(reclaim.TxClosure(res))
	return out
}

// frontierSection classifies g and assembles the disclosed section: the markers,
// the aggregate unconfirmed-route COUNT (not the per-route list — that rides the
// on-demand view), and the coverage caveat. Returns nil when there is nothing to
// disclose, so a clean service emits no section (and absence on an UNSCOPED graph
// honestly means "proven clean").
func frontierSection(g *Graph) *FrontierSection {
	r := frontier.Classify(frontierInput(g))
	if len(r.Markers) == 0 && len(r.UnconfirmedRoutes) == 0 {
		return nil
	}
	return &FrontierSection{
		Markers:           r.Markers,
		UnconfirmedRoutes: len(r.UnconfirmedRoutes),
		Coverage:          frontier.Coverage,
	}
}

// ClassifyFrontier returns the FULL classifier result for g — markers plus the
// per-route unconfirmed list — for the on-demand `flowmap frontier` view, which
// shows detail the committed section deliberately keeps as an aggregate.
func ClassifyFrontier(g *Graph) *frontier.Result { return frontier.Classify(frontierInput(g)) }

// isRouteEntrypoint reports whether an entrypoint kind is a DISCOVERED route — an
// HTTP route or a consumed event — the universe the frontier's route-severance
// analysis (and its attribution_loss ratio) is defined over. Declared callbacks
// and workers (KindCallback/KindWorker) are EXCLUDED: they are author-asserted
// entries, not discovered routes, and a declared worker may be legitimately
// effect-less (a logging-only reconcile loop), so admitting them would manufacture
// false "starved-entrypoint" severances and dilute the attribution_loss denominator.
func isRouteEntrypoint(kind string) bool {
	return kind == string(roots.KindHTTP) || kind == string(roots.KindConsumer)
}

// RouteEntrypointCount is the number of DISCOVERED route entrypoints (HTTP routes
// plus consumed events) — the denominator the frontier's attribution_loss is
// defined over. It excludes declared callbacks/workers so the numerator (severed
// routes) and denominator share one universe; see isRouteEntrypoint.
func (g *Graph) RouteEntrypointCount() int {
	n := 0
	for _, ep := range g.Entrypoints {
		if isRouteEntrypoint(ep.Kind) {
			n++
		}
	}
	return n
}

// frontierInput adapts the assembled graph into the classifier's serialization-free
// input view (frontier imports nothing of graphio; graphio adapts to it). The fold
// state comes from g.foldSQL — the actual build flag, NOT inferred from tagged
// edges, so a --reclaim-sql run that recovers nothing is still reported as folded
// (its remaining opaque-db markers are the genuine residue, not "untried").
func frontierInput(g *Graph) *frontier.Input {
	in := &frontier.Input{Folded: g.foldSQL}
	// The reclaimable-closure set is the `.To` of the in-graph reclaim edges — derived
	// from the one stored edge set so it cannot diverge from what ApplyReclaimers folds.
	// Consumed by Classify as a set, so order is irrelevant.
	for _, e := range g.reclaimEdges {
		in.Reclaimable = append(in.Reclaimable, e.To)
	}
	// The middleware-reclaimable set is the SITE of every loop the middleware-chain reclaimer
	// proves EMPTY (its ResolvedEmpty) — a standalone UnresolvedCall there is binned B
	// (reclaimable by --reclaim-middleware) rather than A. Same single-source discipline:
	// derived from the dry run ApplyMiddlewareReclaimer clears, so prediction == apply.
	for _, s := range g.middlewareReclaim.ResolvedEmpty {
		in.MiddlewareReclaimable = append(in.MiddlewareReclaimable, s.Site)
	}
	for _, n := range g.Nodes {
		in.Nodes = append(in.Nodes, n.FQN)
	}
	for _, e := range g.Edges {
		in.Edges = append(in.Edges, frontier.InEdge{From: e.From, To: e.To})
	}
	for _, b := range g.BlindSpots {
		in.BlindSpots = append(in.BlindSpots, frontier.InBlindSpot{Kind: string(b.Kind), Site: b.Site})
	}
	for _, ep := range g.Entrypoints {
		if !isRouteEntrypoint(ep.Kind) {
			continue // declared callbacks/workers are not routes; see isRouteEntrypoint
		}
		in.Entrypoints = append(in.Entrypoints, frontier.InEntry{Fn: ep.Fn, Name: ep.Name})
	}
	return in
}

// edgeKeySet returns the set of (From, To) keys of g's current edges — the dedup base
// the opt-in fold passes (ApplyReclaimers, ApplyRebind) check against before adding.
func (g *Graph) edgeKeySet() map[[2]string]bool {
	m := make(map[[2]string]bool, len(g.Edges))
	for _, e := range g.Edges {
		m[[2]string{e.From, e.To}] = true
	}
	return m
}

// foldEdge appends one reclaimer/rebind edge (from→to, tagged via) to g as a Tier-2 edge
// IFF both endpoints are graph nodes and the edge is not already present, updating
// `present`. It is the ONE place the node-existence + (From, To) dedup discipline of the
// opt-in fold passes lives (CLAUDE.md "one source of truth"), so ApplyReclaimers and
// ApplyRebind cannot drift on what is foldable. Returns whether the edge was added.
func (g *Graph) foldEdge(from, to, via string, present map[[2]string]bool, nodes map[string]bool) bool {
	key := [2]string{from, to}
	if !nodes[from] || !nodes[to] || present[key] {
		return false
	}
	g.Edges = append(g.Edges, Edge{From: from, To: to, Tier: 2, Via: via})
	present[key] = true
	return true
}

// ApplyReclaimers runs the sound dispatch-seam reclaimers (reclaim package) over
// res and folds the recovered edges into g, re-sorting and re-classifying the
// frontier so it reflects the reclaimed graph. It is OPT-IN (D2): Build never calls
// it, so the default graph — and every committed golden — is unchanged; a caller
// asks for it explicitly (`flowmap graph --reclaim`). Each added edge is one real
// execution can take (R2) and carries its reclaimer in Via, so a reviewer can diff
// base-vs-reclaimed. Returns the number of edges added. Only edges between existing
// nodes that are not already present are folded in.
func ApplyReclaimers(g *Graph, res *analyze.Result) int {
	present := g.edgeKeySet()
	nodes := g.nodeSet()
	// Reuse the in-graph reclaim edges Build already computed (the unscoped case, where
	// a frontier rides); a scoped build never computed them, so recompute there. Either
	// way the SAME helper produces the set, so StrictServer runs ONCE on the common
	// unscoped path and the folded edges match the frontier's reclaimable prediction.
	edges := g.reclaimEdges
	if g.Entrypoint != "" {
		edges = reclaimEdges(res, nodes)
	}
	added := 0
	for _, e := range edges {
		if g.foldEdge(e.From, e.To, e.Via, present, nodes) {
			added++
		}
	}
	if added > 0 {
		sortGraph(g)
		// Re-classify only for an unscoped graph — the frontier section is a
		// whole-service disclosure (see Build), so a scoped reclaim re-sorts its
		// edges but carries no frontier.
		if g.Entrypoint == "" {
			g.Frontier = frontierSection(g)
		}
	}
	return added
}

// ApplyMiddlewareReclaimer runs the middleware-chain reclaimer (reclaim.MiddlewareChain)
// over res and folds the result into g: it ADDs the recovered edges (tagged
// via=middleware-chain) that resolve the oapi-codegen / chi middleware-application loop,
// and DROPs the UnresolvedCall blind spots at loops whose middleware set is provably empty
// (so the loop body is dead and hides nothing). It is OPT-IN (`flowmap graph
// --reclaim-middleware`): Build never calls it, so the default graph — every committed
// golden — is unchanged. Each added edge is one real execution can take (R2) and carries
// its reclaimer in Via. A blind spot is dropped only when its (Site, type) matches a
// fully-resolved empty seam, so a non-empty middleware loop (or a same-function func-value
// call of another type) stays disclosed. Returns the counts of edges added and blind spots
// cleared; re-sorts and re-classifies the frontier like ApplyReclaimers when anything changed.
func ApplyMiddlewareReclaimer(g *Graph, res *analyze.Result) (added, cleared int) {
	// Reuse the dry run Build already computed (the unscoped case, where a frontier rides);
	// a scoped build never computed it, so recompute there. Either way the SAME helper
	// produces it, so the folded edges and cleared seams match the frontier's prediction.
	mw := g.middlewareReclaim
	if g.Entrypoint != "" {
		mw = reclaim.MiddlewareChain(res)
	}
	present := g.edgeKeySet()
	nodes := g.nodeSet()
	for _, e := range mw.Edges {
		if g.foldEdge(e.From, e.To, e.Via, present, nodes) {
			added++
		}
	}
	if len(mw.ResolvedEmpty) > 0 {
		cleared = dropResolvedSeams(g, mw.ResolvedEmpty)
	}
	if added > 0 || cleared > 0 {
		sortGraph(g)
		// Re-classify only for an unscoped graph — the frontier section is a whole-service
		// disclosure (see Build), so a scoped reclaim re-sorts its edges but carries no frontier.
		if g.Entrypoint == "" {
			g.Frontier = frontierSection(g)
		}
	}
	return added, cleared
}

// dropResolvedSeams removes from g.BlindSpots every UnresolvedCall whose (Site, defined type
// named in Detail) matches a fully-resolved empty middleware seam, returning how many were
// dropped. The match is on BOTH the site (the loop function's FQN) AND the element type
// (the type the reclaimer resolved), so an UnresolvedCall at the same site for a different
// func type — or a middleware loop whose set was NOT proven empty — survives untouched. The
// seam is the one disclosure the reclaimer is allowed to retract, and only because an empty
// set means the loop hides nothing and the pass-through handler edge was recovered.
func dropResolvedSeams(g *Graph, seams []reclaim.MiddlewareSeam) int {
	cleared := 0
	kept := g.BlindSpots[:0]
	for _, b := range g.BlindSpots {
		if b.Kind == blindspots.UnresolvedCall && matchesResolvedSeam(b, seams) {
			cleared++
			continue
		}
		kept = append(kept, b)
	}
	g.BlindSpots = kept
	return cleared
}

// matchesResolvedSeam reports whether the blind spot b is the UnresolvedCall a resolved empty
// seam retracts: same site, and the seam's element type named in b's Detail. The type is
// matched as the ANCHORED token blindspots writes ("of type <T> resolved to no callee"), NOT a
// bare substring: a bare strings.Contains would let a seam for type `pkg.MW` clear an unrelated
// UnresolvedCall of `pkg.MWAudit` at the same site (one name is a substring of the other),
// silently retracting a live disclosure — a false PROVEN. The framing brackets the type on both
// sides, so only the exact type clears. If blindspots ever changes that prose the match fails
// closed (the seam stays disclosed), never the reverse.
func matchesResolvedSeam(b blindspots.BlindSpot, seams []reclaim.MiddlewareSeam) bool {
	for _, s := range seams {
		if b.Site == s.Site && strings.Contains(b.Detail, "of type "+s.TypeName+" resolved to no callee") {
			return true
		}
	}
	return false
}

// ApplyRebind runs the EXPERIMENTAL de-union pass (rebind package) over res and folds
// the result into g: it ADDs each command's precise enclosing-fn→closure edge (tagged
// via=rebind) and REMOVEs the shared runner→closure union edges. It is the ONLY graph
// mutation that removes edges (the soundness-dangerous direction), so it is opt-in and
// experimental (`flowmap graph --rebind`): Build never calls it, and the default graph —
// every committed golden — is unchanged. The removal is sound because the rebind pass
// abstains (keeps the union) on any closure that escapes its parent. Only BASE union
// edges (no Via) are removed, so a reclaimed or already-rebound edge is never dropped.
// Returns the counts of edges added and removed; re-sorts and re-classifies like
// ApplyReclaimers when anything changed.
func ApplyRebind(g *Graph, res *analyze.Result) (added, removed int) {
	plan := rebind.Compute(res)
	present := g.edgeKeySet()
	nodes := g.nodeSet()

	for _, e := range plan.Add {
		if g.foldEdge(e.From, e.To, rebind.Via, present, nodes) {
			added++
		}
	}

	remove := make(map[[2]string]bool, len(plan.Remove))
	for _, p := range plan.Remove {
		remove[p] = true
	}
	if len(remove) > 0 {
		kept := g.Edges[:0]
		for _, e := range g.Edges {
			// Drop only BASE union edges (Via empty) that the plan targets; a reclaimed
			// or rebound edge is never removed.
			if e.Via == "" && remove[[2]string{e.From, e.To}] {
				removed++
				continue
			}
			kept = append(kept, e)
		}
		g.Edges = kept
	}

	if added > 0 || removed > 0 {
		sortGraph(g)
		if g.Entrypoint == "" {
			g.Frontier = frontierSection(g)
		}
	}
	return added, removed
}

// obligationSummaries hands the engine its production inputs (CX-2): the
// whole built program (NewProgramSummaries owns the universe-completeness
// precondition — package initializers run before main and can take addresses
// or call in without being RTA-rooted) and the call graph's edges as the
// per-site over-approximation, unfiltered. Built only when obligation rules
// or effect labels exist; rule-free, effect-free services pay nothing.
func obligationSummaries(res *analyze.Result) *obligations.Summaries {
	bySite := map[ssa.CallInstruction][]*ssa.Function{}
	for _, n := range res.Graph.Nodes {
		for _, e := range n.Out {
			if e.Site != nil {
				bySite[e.Site] = append(bySite[e.Site], e.Callee.Func)
			}
		}
	}
	return obligations.NewProgramSummaries(res.Program.Prog,
		func(site ssa.CallInstruction) []*ssa.Function { return bySite[site] })
}

// Marshal renders the graph as canonical JSON (non-gated, but still deterministic).
func (g *Graph) Marshal() ([]byte, error) { return canonjson.Marshal(g) }

// EntryNotFoundError reports that no entry-point root matched a --entry argument.
type EntryNotFoundError struct{ Entry string }

func (e *EntryNotFoundError) Error() string { return "no entry point named " + e.Entry }

// EntryAmbiguousError reports that a --entry name matched more than one DISTINCT
// root handler. The build refuses rather than root the scope at whichever was
// registered first: a wrong-but-plausible scope is exactly the silent-wrong
// outcome the toolchain must not produce (CLAUDE.md: fail closed). Fns are the
// distinct matching handler FQNs, sorted.
type EntryAmbiguousError struct {
	Entry string
	Fns   []string
}

func (e *EntryAmbiguousError) Error() string {
	return fmt.Sprintf("entry %q is ambiguous: it names %d distinct handlers (%s); disambiguate with an exact function FQN", e.Entry, len(e.Fns), strings.Join(e.Fns, ", "))
}

// spliceDepthCap bounds edgeOf's recursion through chained $bound/$thunk wrappers.
// Real wrapper chains are at most ~2 deep; the cap only exists so a pathological
// cycle fails closed (drops the edge) instead of recurring forever.
const spliceDepthCap = 16

// edgeOf renders graph edges for an SSA call edge: a typed boundary edge for
// publish/HTTP/DB calls, an internal edge for first-party→first-party calls, the
// spliced edges of a $bound/$thunk wrapper's target, and nothing for calls into
// unhinted stdlib/third-party code.
func edgeOf(ext *features.Extractor, hints *features.HintSet, e *cg.Edge, scope map[*ssa.Function]bool, foldSQL, foldBus bool, callers map[*ssa.Function][]ssa.CallInstruction, depth int) []Edge {
	from := e.Caller.Func.RelString(nil)
	callee := e.Callee.Func
	f := ext.Edge(e.Caller.Func, callee, e.Site)
	tier, _ := ext.Classify(f)
	concurrent := f.Concurrent

	switch {
	case isSplicedWrapper(callee):
		// Splice a thin $bound/$thunk wrapper: the caller reaches whatever the wrapper
		// forwards to, so re-attribute the wrapper's out-edges to `from` and classify
		// caller→wrappee normally. This keeps the real method (and any boundary effect
		// inside or below it) connected WITHOUT rendering a synthetic "$bound" node
		// (C-1). The original call's concurrency is carried onto the spliced edges: a
		// `go methodValue()` reaches the wrappee concurrently even though the wrapper's
		// own body calls it directly. depth bounds the (in practice ≤2) wrapper chain so
		// a pathological cycle fails closed instead of recurring forever.
		if depth > spliceDepthCap {
			return nil
		}
		var out []Edge
		for _, oe := range e.Callee.Out {
			spliced := &cg.Edge{Caller: e.Caller, Callee: oe.Callee, Site: oe.Site}
			out = append(out, edgeOf(ext, hints, spliced, scope, foldSQL, foldBus, callers, depth+1)...)
		}
		if concurrent {
			for i := range out {
				out[i].Concurrent = true
			}
		}
		return out
	case features.IsPackageInit(callee):
		// A call to a package initializer is init-ordering plumbing: the
		// synthesized `init` of every package calls the `init` of each package it
		// imports. It is never a real boundary operation, so it must NOT be
		// classified by the package-keyed hints — without this guard, rooting
		// init() turns `store.init -> database/sql.init` into a spurious
		// "boundary:db init" effect (a false write in the canonical IR). The
		// internal edge is kept when the callee is first-party so init reachability
		// still propagates; a crossing into stdlib/third-party renders nothing.
		if scope[callee] {
			return []Edge{{From: from, To: callee.RelString(nil), Tier: tier, Concurrent: concurrent}}
		}
		return nil
	case hints.IsPublish(callee):
		return busEdges(from, "boundary:bus PUBLISH ", e.Site, foldBus, tier, string(f.Boundary), concurrent)
	case hints.IsConsume(callee):
		return busEdges(from, "boundary:bus CONSUME ", e.Site, foldBus, tier, string(f.Boundary), concurrent)
	case hints.IsHTTP(callee):
		return []Edge{{From: from, To: "boundary:" + httpLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
	case hints.IsDB(callee):
		// §19 Tier-B/C: when the sink forwards a bare query PARAMETER, the verb is
		// invisible here — it lives one call-hop up at each caller — so re-attribute
		// the effect to the callers (classified where the caller's SQL is recoverable,
		// else the opaque label re-homed). Sound-or-abstain: it fails closed to the
		// normal opaque helper edge unless every caller is accounted for.
		if foldSQL {
			if redges, ok := passthroughReattribute(ext, e, tier, string(f.Boundary), concurrent, scope, callers); ok {
				return redges
			}
		}
		labels, via := dbLabel(e.Site, foldSQL)
		edges := make([]Edge, 0, len(labels))
		for _, label := range labels {
			edges = append(edges, Edge{From: from, To: boundarylabel.DBPrefix + label, Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent, Via: via})
		}
		return edges
	case methodNamedOutboundKind(hints, callee) != "":
		// A method-named outbound kind (blob/cache/rpc) carries no readable
		// peer/op/target triple, so the operation is the callee method name
		// (sinkMethodName — the same opaque label db falls back to), e.g.
		// "boundary:blob PutObject", "boundary:cache Get", "boundary:rpc Charge".
		kind := methodNamedOutboundKind(hints, callee)
		return []Edge{{From: from, To: "boundary:" + kind + " " + sinkMethodName(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
	case scope[callee]:
		return []Edge{{From: from, To: callee.RelString(nil), Tier: tier, Concurrent: concurrent}}
	default:
		return nil // a call into unhinted stdlib/third-party code; not part of the view
	}
}

// methodNamedOutboundKind returns the blob/cache/rpc kind token for callee, or ""
// if it is not a method-named outbound effect. A thin string-returning wrapper over
// the hint helper so the edgeOf switch can both test and use the kind in one place.
func methodNamedOutboundKind(hints *features.HintSet, callee *ssa.Function) string {
	kind, _ := hints.MethodNamedOutboundKind(callee)
	return kind
}

// busEdges builds the boundary edge(s) for a bus PUBLISH/CONSUME site. The topic
// labeler (eventLabels) returns one name normally, or — under --reclaim-topic — a
// finite constant set that fans out into one edge per topic (an over-approximation in
// the safe direction, mirroring the DB table fanout). Every edge carries the same via
// provenance so a reviewer can tell a reclaimed target from a base one.
func busEdges(from, prefix string, site ssa.CallInstruction, foldBus bool, tier int, boundary string, concurrent bool) []Edge {
	labels, via := eventLabels(site, foldBus)
	edges := make([]Edge, 0, len(labels))
	for _, label := range labels {
		edges = append(edges, Edge{From: from, To: prefix + label, Tier: tier, Boundary: boundary, Concurrent: concurrent, Via: via})
	}
	return edges
}

// callerIndex maps each function to the call instructions that invoke it, over the
// whole call graph (an over-approximation: it includes every real caller, so
// enumerating callers from it can never MISS one — the soundness precondition for
// re-attributing a callee's effect to its callers). Built only under --reclaim-sql.
func callerIndex(res *analyze.Result) map[*ssa.Function][]ssa.CallInstruction {
	idx := map[*ssa.Function][]ssa.CallInstruction{}
	for _, n := range res.Graph.Nodes {
		for _, e := range n.Out {
			if e.Site != nil && e.Callee != nil && e.Callee.Func != nil {
				idx[e.Callee.Func] = append(idx[e.Callee.Func], e.Site)
			}
		}
	}
	return idx
}

// passthroughReattribute implements the §19 Tier-B/C reclaimer. When a DB sink's
// query argument is a bare PARAMETER of the enclosing helper, the helper forwards
// the statement unmodified, so its verb is invisible at the sink — it lives one
// call-hop up, at each caller that built the statement. This re-attributes the
// effect to those callers: for each it recovers the SQL passed (a call-site
// constant or the const-accumulation fold, which also resolves a finite-constant
// table set — Tier C) and emits a boundary edge from the CALLER, classified when
// recoverable, else the sink's opaque method-name label re-homed at the caller. The
// helper's own (necessarily opaque) sink edge is dropped, since every caller now
// carries the effect — the effect surface is preserved, never hidden.
//
// It returns ok=false (so edgeOf emits the normal opaque helper edge) unless it can
// soundly account for EVERY caller: each must be an in-scope, statically resolved
// call whose argument list reaches the parameter slot. A single unaccountable caller
// (an interface invoke, an out-of-scope caller, a truncated arg list) means
// re-attribution could hide the helper's effect, so it fails closed — the
// prime-directive choice (a disclosed opaque effect beats a silently dropped write).
func passthroughReattribute(ext *features.Extractor, e *cg.Edge, tier int, boundary string, concurrent bool, scope map[*ssa.Function]bool, callers map[*ssa.Function][]ssa.CallInstruction) ([]Edge, bool) {
	helper := e.Caller.Func
	sink := e.Site
	if sink == nil {
		return nil, false
	}
	qargs := features.StringArgs(sink)
	if len(qargs) == 0 {
		return nil, false
	}
	param, ok := qargs[0].(*ssa.Parameter)
	if !ok || param.Parent() != helper {
		return nil, false // the sink's query is not a bare forwarded parameter
	}
	paramIdx := -1
	for i, p := range helper.Params {
		if p == param {
			paramIdx = i
			break
		}
	}
	if paramIdx < 0 {
		return nil, false
	}
	sites := callers[helper]
	if len(sites) == 0 {
		return nil, false // nobody to attribute to → keep the opaque helper edge
	}
	opaque := sinkMethodName(sink)
	seen := map[[2]string]int{}
	var out []Edge
	// A re-attributed effect is concurrent if EITHER the helper→sink call is
	// concurrent OR the caller dispatches the helper concurrently (`go helper(...)`).
	// Inheriting the caller→helper dispatch concurrency is what closes the §19
	// cone gap: when the leaf helper itself is spawned, the effect now lives at the
	// caller (upstream of the spawned cone), so checkNoConcurrentReach's cone path
	// cannot see it — but the edge's own Concurrent flag (its direct-boundary path)
	// now does. emit OR-s concurrency across duplicate (from,label) edges so a
	// racy path is never masked by a synchronous one.
	emit := func(from, label string, conc bool) {
		key := [2]string{from, label}
		if i, ok := seen[key]; ok {
			if conc {
				out[i].Concurrent = true
			}
			return
		}
		seen[key] = len(out)
		out = append(out, Edge{From: from, To: boundarylabel.DBPrefix + label, Tier: tier, Boundary: boundary, Concurrent: conc, Via: viaPassthrough})
	}
	keepHelper := false
	for _, site := range sites {
		callerFn := site.Parent()
		if callerFn == nil || !scope[callerFn] {
			// An out-of-scope caller is not a graph node, so its effect cannot be homed
			// at it; preserve the surface by keeping the helper's own opaque edge rather
			// than dropping a reachable effect.
			keepHelper = true
			continue
		}
		from := callerFn.RelString(nil)
		// concurrent OR the dispatch's own concurrency, taken from the SAME extractor
		// the rest of the graph uses (one definition of "concurrent", no drift).
		conc := concurrent || ext.Edge(callerFn, helper, site).Concurrent
		// Sound arg mapping needs a statically-resolved DIRECT call whose Args align
		// with the callee's Params (Args[0] is the receiver for a method, matching
		// Params[0]) AND whose slot type matches the parameter — the type check rejects
		// a misaligned generic/thunk arity that would otherwise read an unrelated arg.
		if site.Common().StaticCallee() == helper {
			args := site.Common().Args
			if paramIdx < len(args) && types.Identical(args[paramIdx].Type(), param.Type()) {
				if labels, _, ok := recoverDBLabelsFromValue(args[paramIdx], true); ok {
					for _, l := range labels {
						emit(from, l, conc)
					}
					continue
				}
			}
		}
		// Mapped-but-unrecoverable (dynamic SQL) OR unmappable (invoke / misaligned):
		// re-home the opaque label at the caller — the effect is preserved and
		// disclosed as unclassified, just not classified. This is per-caller fail
		// closed: one such caller does not collapse re-attribution for its siblings.
		emit(from, opaque, conc)
	}
	if keepHelper {
		emit(helper.RelString(nil), opaque, concurrent)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// committedEffect reports whether a boundary label is a committed external
// effect for partial-effect purposes: a bus publish or a DB mutation. Reads
// and outbound queries are not "committed" — re-running them is safe.
func committedEffect(label string) bool {
	if event, ok := strings.CutPrefix(label, "boundary:bus PUBLISH "); ok {
		// A dynamic (non-constant) event name is NOT a concretely-named committed
		// effect — symmetric to the unreadable-SQL DB op below, it is disclosed
		// via the dynamic/blind-spot channel rather than asserted as a definite
		// publish of a known event.
		return event != dynamicLabel
	}
	if strings.HasPrefix(label, boundarylabel.DBPrefix) {
		op := strings.Fields(strings.TrimPrefix(label, boundarylabel.DBPrefix))
		return len(op) > 0 && mutatingSQLOp(op[0])
	}
	return false
}

// mutatingSQLOp reports whether a SQL verb the labeler read off a constant
// statement commits a row mutation. The verb set lives in sqlverb (the single
// source of truth shared with fitness.IsWrite), so the partial-effect disclosure
// here and the I/O-budget write surface there cannot drift. A DB op the labeler
// could NOT read (a dynamic statement that fell back to the driver method name,
// e.g. "Exec") is deliberately NOT treated as committed here: it flows through
// the separate unclassified-DB-label caution channel instead of being silently
// asserted as a definite write.
func mutatingSQLOp(op string) bool {
	return sqlverb.Mutating(op)
}

// nodeTier ranks a function by what it does, not by what it is. A root is its
// inbound entry tier. Every other function takes the min over its direct
// outgoing edge tiers, falling back to the function's own compute floor (its
// internal same-package self-edge, tier 3 by default) when it reaches no
// consequential boundary. This is direct, not transitive — a helper that
// performs a DB read surfaces as tier 2 and one that publishes as tier 1, while a
// function that merely calls such helpers does not inherit their tier (so
// salience does not propagate up from main). Without this, classifying a function
// by its self-edge alone left every non-root function stuck at the compute floor.
func nodeTier(ext *features.Extractor, fn *ssa.Function, isRoot bool, outEdges []Edge) int {
	if isRoot {
		t, _ := ext.Classify(ext.Inbound(fn.RelString(nil), fallible(fn)))
		return t
	}
	// The self-edge (fn→fn) is the function's compute floor: internal,
	// same-package, no effect — tier 3 under the default rules. A function that is
	// itself a declared boundary sink (a named classify.db/http/publish hint, e.g.
	// loansvc's client.Call) intentionally surfaces at its boundary tier here, since
	// the hint switch keys on the callee and callee==fn — that is how a boundary
	// function shows up salient even when all its outgoing edges are compute.
	tier, _ := ext.Classify(ext.Edge(fn, fn, nil))
	for _, e := range outEdges {
		if e.Tier < tier {
			tier = e.Tier
		}
	}
	return tier
}

func sortGraph(g *Graph) {
	// Total order over the node's intrinsic fields, not FQN alone: a generic
	// instance's display FQN (fn.RelString) is documented non-unique, so an
	// FQN-only comparator left two same-named instances in build (map-iteration)
	// order — a latent nondeterminism that went live the moment instances entered
	// the rendered scope (C-1/M-20). Sig carries the instantiated signature (its
	// concrete type args), so it disambiguates instances; Package/File/Line break
	// any residual tie on run-independent data.
	sort.Slice(g.Nodes, func(i, j int) bool {
		a, b := g.Nodes[i], g.Nodes[j]
		if a.FQN != b.FQN {
			return a.FQN < b.FQN
		}
		if a.Sig != b.Sig {
			return a.Sig < b.Sig
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	// Total order over every Edge field: a comparator that ignored Boundary and
	// Concurrent left equal-keyed edges in build order — deterministic only as
	// long as the pre-sort slice happened to be, a latent output-stability trap.
	// Via is included for the same reason (dedupEdges compares full struct equality,
	// so a comparator that omitted Via could order two Via-differing edges by build
	// order while dedup kept both — a stability gap if a future reclaimer emits a
	// Via edge parallel to a base From/To).
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Boundary != b.Boundary {
			return a.Boundary < b.Boundary
		}
		if a.Concurrent != b.Concurrent {
			return !a.Concurrent && b.Concurrent
		}
		return a.Via < b.Via
	})
	g.Edges = dedupEdges(g.Edges)
}

func dedupEdges(in []Edge) []Edge {
	out := in[:0]
	var prev Edge
	for i, e := range in {
		if i > 0 && e == prev {
			continue
		}
		out = append(out, e)
		prev = e
	}
	return out
}
