// Package graphio renders the static pipeline's NON-gated view: the full
// first-party call graph with signatures and typed boundary edges (static-
// extractor spec §2, §9). Unlike the boundary contract, this view DOES include DB
// edges and internal call structure — it is the richer "what can happen" map for
// human understanding and the AI-assist surface. It is regenerated on demand and
// never gated, because function-level structure churns under refactoring.
package graphio

import (
	"fmt"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	cg "github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/frontier"
	"github.com/jyang234/golang-code-graph/internal/static/obligations"
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
	// Each is matched to a detected blind spot at build time (an unmatched one fails
	// the build — drift, not a silent drop). Omitted when none, so an annotation-free
	// service emits a byte-identical graph.
	Annotations []Annotation `json:"annotations,omitempty"`

	// foldSQL records whether this graph was built with the SQL const-fold
	// (--reclaim-sql / WithSQLFold). Unexported, so it never serializes (no golden
	// churn) and is an in-memory build flag only: the frontier classifier reads it
	// to split the opaque-db disclosure into B2a/B2b, taken from the flag rather than
	// inferred from tagged edges so an all-abstain fold run still reads as folded.
	foldSQL bool
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

// Entrypoint is one named root: an HTTP route or a consumed topic, with the
// function registered to handle it.
type Entrypoint struct {
	Kind string `json:"kind"` // "http" or "consumer"
	Name string `json:"name"` // "POST /loan-application", "/transfer", "payment.settled"
	Fn   string `json:"fn"`
}

// Node is one first-party function.
type Node struct {
	FQN      string `json:"fqn"`
	Sig      string `json:"sig"`
	Tier     int    `json:"tier"`
	Fallible bool   `json:"fallible,omitempty"`
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
	Site string `json:"site"`
	Kind string `json:"kind"`
	Note string `json:"note"`
	By   string `json:"by,omitempty"`
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
func mergeAnnotations(manifest []blindspots.BlindSpot, cfg *config.Config) ([]Annotation, error) {
	if cfg == nil || len(cfg.Static.Annotations) == 0 {
		return nil, nil
	}
	// Index the manifest: the kinds present at each site (deduped by the resolver).
	kindsAt := map[string][]string{}
	for _, b := range manifest {
		kindsAt[b.Site] = append(kindsAt[b.Site], string(b.Kind))
	}
	byKey := map[[2]string]Annotation{}
	for i, a := range cfg.Static.Annotations {
		// The binding rule lives in config.ResolveAnnotationKind so the producer here
		// and the read-only MCP `annotate` proposer cannot drift (parity test below).
		kind, err := config.ResolveAnnotationKind(a.Site, a.Kind, kindsAt[a.Site])
		if err != nil {
			return nil, fmt.Errorf("flowmap config: static.annotations[%d]: %w", i, err)
		}
		// Collapse duplicate (site, kind) annotations to the lexically-smallest note
		// — an intrinsic tie-break, never arrival order (CLAUDE.md determinism).
		cand := Annotation{Site: a.Site, Kind: kind, Note: a.Note, By: a.By}
		key := [2]string{a.Site, kind}
		if cur, ok := byKey[key]; !ok || cand.Note < cur.Note {
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
	return out, nil
}

// Build renders the full first-party graph of res. If entry is non-empty, the
// graph is scoped to the functions reachable from the matching entry-point root.
// BuildOption tunes a graph build. Options are opt-in refinements (D2-style): the
// zero set reproduces the default graph byte-for-byte, so every committed golden
// is unchanged unless a caller explicitly asks for a refinement.
type BuildOption func(*buildOptions)

type buildOptions struct {
	foldSQL bool
}

// WithSQLFold enables the SQL const-accumulation label reclaimer (--reclaim-sql):
// DB effects whose statement is non-constant at the call site but provably built
// from constant fragments get their verb recovered and the edge tagged
// via=sqlfold.Via. Off by default; sound-or-abstain (docs/design/
// sql-constfold-reclaim-plan.md).
func WithSQLFold() BuildOption { return func(o *buildOptions) { o.foldSQL = true } }

func Build(res *analyze.Result, entry string, opts ...BuildOption) (*Graph, error) {
	var o buildOptions
	for _, opt := range opts {
		opt(&o)
	}
	ext := features.NewExtractor(res.Config, res.Program.ModulePath)
	hints := ext.Hints()

	scope := firstPartyScope(res)
	if entry != "" {
		root := rootByName(res, entry)
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
	// channel still fails closed on drift, never attaching context to a vanished site.
	annots, err := mergeAnnotations(g.BlindSpots, res.Config)
	if err != nil {
		return nil, err
	}
	g.Annotations = annots
	rootFns := rootFuncSet(res)
	if entry == "" {
		for _, r := range res.Roots.Roots {
			if r.Name == "" || !scope[r.Func] {
				continue
			}
			if r.Kind == roots.KindHTTP || r.Kind == roots.KindConsumer {
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
	}
	base := ""
	if entry == "" {
		abs, err := filepath.Abs(res.Dir)
		if err != nil {
			return nil, err
		}
		base = abs
	}

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
			edges := edgeOf(ext, hints, e, scope, o.foldSQL, callers)
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
		g.Nodes = append(g.Nodes, Node{
			FQN:      fn.RelString(nil),
			Sig:      signatures.Of(fn),
			Tier:     nodeTier(ext, fn, rootFns[fn], nodeEdges),
			Fallible: fallible(fn),
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
		g.Frontier = frontierSection(g)
	}
	return g, nil
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

// frontierInput adapts the assembled graph into the classifier's serialization-free
// input view (frontier imports nothing of graphio; graphio adapts to it). The fold
// state comes from g.foldSQL — the actual build flag, NOT inferred from tagged
// edges, so a --reclaim-sql run that recovers nothing is still reported as folded
// (its remaining opaque-db markers are the genuine residue, not "untried").
func frontierInput(g *Graph) *frontier.Input {
	in := &frontier.Input{Folded: g.foldSQL}
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
		in.Entrypoints = append(in.Entrypoints, frontier.InEntry{Fn: ep.Fn, Name: ep.Name})
	}
	return in
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
	nodes := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		nodes[n.FQN] = true
	}
	existing := make(map[[2]string]bool, len(g.Edges))
	for _, e := range g.Edges {
		existing[[2]string{e.From, e.To}] = true
	}
	added := 0
	for _, e := range reclaim.StrictServer(res) {
		if !nodes[e.From] || !nodes[e.To] || existing[[2]string{e.From, e.To}] {
			continue
		}
		g.Edges = append(g.Edges, Edge{From: e.From, To: e.To, Tier: 2, Via: e.Via})
		existing[[2]string{e.From, e.To}] = true
		added++
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

// edgeOf renders zero or one graph edges for an SSA call edge: a typed boundary
// edge for publish/HTTP/DB calls, an internal edge for first-party→first-party
// calls, and nothing for calls into unhinted stdlib/third-party code.
func edgeOf(ext *features.Extractor, hints *features.HintSet, e *cg.Edge, scope map[*ssa.Function]bool, foldSQL bool, callers map[*ssa.Function][]ssa.CallInstruction) []Edge {
	from := e.Caller.Func.RelString(nil)
	callee := e.Callee.Func
	f := ext.Edge(e.Caller.Func, callee, e.Site)
	tier, _ := ext.Classify(f)
	concurrent := f.Concurrent

	switch {
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
		return []Edge{{From: from, To: "boundary:bus PUBLISH " + eventLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
	case hints.IsConsume(callee):
		return []Edge{{From: from, To: "boundary:bus CONSUME " + eventLabel(e.Site), Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent}}
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
			edges = append(edges, Edge{From: from, To: "boundary:db " + label, Tier: tier, Boundary: string(f.Boundary), Concurrent: concurrent, Via: via})
		}
		return edges
	case scope[callee]:
		return []Edge{{From: from, To: callee.RelString(nil), Tier: tier, Concurrent: concurrent}}
	default:
		return nil // a call into unhinted stdlib/third-party code; not part of the view
	}
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
		out = append(out, Edge{From: from, To: "boundary:db " + label, Tier: tier, Boundary: boundary, Concurrent: conc, Via: viaPassthrough})
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
	if strings.HasPrefix(label, "boundary:db ") {
		op := strings.Fields(strings.TrimPrefix(label, "boundary:db "))
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
	// same-package, no effect — tier 3 under the default rules.
	tier, _ := ext.Classify(ext.Edge(fn, fn, nil))
	for _, e := range outEdges {
		if e.Tier < tier {
			tier = e.Tier
		}
	}
	return tier
}

func sortGraph(g *Graph) {
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].FQN < g.Nodes[j].FQN })
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
