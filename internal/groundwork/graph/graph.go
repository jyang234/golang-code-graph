// Package graph loads and indexes the call graph that flowmap emits (`flowmap
// graph <service>`) and is the substrate every groundwork surface is built on.
//
// groundwork deliberately declares its own value types here rather than import
// flowmap's internal graphio: the graph JSON is the *interface* between the two
// programs (flowmap produces it, groundwork consumes it), and keeping a separate,
// explicit decode of that interface is what lets the two sit in different trust
// domains — flowmap runs in trusted CI, groundwork only ever reads the file it is
// handed. The shapes are kept in lockstep with graphio by the committed goldens
// under testdata/groundwork and the regen script beside them.
package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/groundwork/setutil"
)

// boundaryPrefix marks an edge target that is a typed external sink (a DB op, an
// outbound call, a bus publish/consume) rather than a first-party function. Such
// targets never appear in Nodes; they are the leaves of the effect surface.
const boundaryPrefix = "boundary:"

// dynamicMarker is flowmap's token for a boundary effect whose target could not
// be named statically (e.g. a publish to a topic chosen at runtime). An edge
// whose target contains it is a known hole in the graph's knowledge — the
// frontier where reachability stops being sound.
const dynamicMarker = "<dynamic>"

// Graph is one call-graph view as emitted by `flowmap graph`. It is the whole,
// unscoped service graph unless Entrypoint is set.
type Graph struct {
	// Stamp is the producer's caller-supplied identity (typically the deployed
	// commit SHA). Consumers pass --expect to verify they hold the graph for
	// the code they think they do — a stale map mis-triages. Opt-in at both
	// ends: an absent stamp is only an error when verification was asked for.
	Stamp string `json:"stamp,omitempty"`

	// Tool is the flowmap build that produced this graph (flowmap's buildinfo
	// version). Where Stamp identifies the CODE, Tool identifies the PRODUCER —
	// provenance the consumer round-trips and verify/review compare across the
	// base/branch pair, because "same code → same graph" determinism holds only
	// within one tool version. A base built by tool A and a branch by tool B (same
	// code, same stamp, same algo) can diff on a pure tool artifact — a relabeled
	// effect, an SSA-order shift — so a base↔branch Tool mismatch is disclosed as a
	// caveat (R11), the Algo/Caveats provenance discipline extended to the producer.
	// Absent means unrecorded (a pre-Tool flowmap) — never silently "same tool".
	Tool       string `json:"tool,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`

	// Algo is the call-graph construction algorithm flowmap built this graph on
	// (rta|vta|cha) and Caveats are its recorded soundness/precision notes —
	// provenance, not gate inputs. fitness/review/verify echo them so a verdict
	// self-certifies its substrate. All three algorithms are sound over-
	// approximations modulo the reflection/unsafe frontier already carried in
	// BlindSpots, so a proof (must_not_reach PROVEN-ABSENT) is valid on any of
	// them; recording which one ran is for auditability. Absent on graphs from a
	// pre-provenance flowmap — an empty Algo means "unrecorded", never "unsound".
	Algo    string   `json:"algo,omitempty"`
	Caveats []string `json:"caveats,omitempty"`

	Nodes       []Node            `json:"nodes"`
	Edges       []Edge            `json:"edges"`
	BlindSpots  []BlindSpot       `json:"blind_spots"`
	Obligations []Obligation      `json:"obligations,omitempty"`
	EffectOrder []EffectOrderFact `json:"effect_order,omitempty"`
	Entrypoints []Entrypoint      `json:"entrypoints,omitempty"`

	// CompositionRoots are the import paths of the producer's `package main`
	// commands (flowmap's authoritative main-package set, roots.KindMain).
	// groundwork decodes it on its own side of the trust boundary —
	// DisallowUnknownFields would reject the graph otherwise — and round-trips it.
	// It is a disclosure (no verdict reads it), but the layering proposer
	// (fitness.proposeLayers) PREFERS it over its structural `.main` heuristic to
	// pick the composition roots it exempts, so the assembly point is the one the
	// producer's SSA identified rather than an FQN guess. Omitted for a library (no
	// command) or a graph from a pre-field flowmap, where the proposer falls back to
	// the heuristic.
	CompositionRoots []string `json:"composition_roots,omitempty"`

	// OmittedPackages are the first-party packages a C3 component imports but that
	// declare no functions (types/consts only) — the imported-but-invisible internal
	// packages flowmap's component rollup discloses. groundwork decodes it on its own
	// side of the trust boundary — DisallowUnknownFields would reject the graph
	// otherwise — and round-trips it. A disclosure: no verdict, count, or reachability
	// computation reads it. Omitted for a service with no types-only internal package,
	// or a graph from a pre-field flowmap.
	OmittedPackages []string `json:"omitted_packages,omitempty"`

	// Frontier is the producer's classification of where static reachability stops
	// (flowmap's frontier section). groundwork decodes it on its own side of the
	// trust boundary — like every other graph-carried section — so a consumer can
	// READ the disclosed frontier (which routes are severed, which writes are opaque)
	// AND the unconfirmed-route count + coverage caveat, instead of reconstructing it
	// from topology. It is a disclosure: no verdict reads it. Omitted when there is
	// nothing to disclose.
	Frontier *FrontierSection `json:"frontier,omitempty"`

	// Annotations are human/AI context attached to blind spots (keyed by Site/Kind),
	// decoded on this side of the trust boundary like every other section. It is a
	// disclosure: NO verdict reads it — it explains a blind spot, it does not change
	// one. groundwork decodes it so the field is known to the strict reader (an
	// unknown field would otherwise be rejected) and so review/triage can echo the
	// context beside the blind spot. Omitted when none.
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Annotation is operator/agent context on a blind spot, keyed by (Site, Kind).
// Disclosure only: no verdict, count, or reachability computation reads it. Claim,
// when set, is the canonical effect key the note asserts is behind the seam — a
// falsifiable form the impeach lens grades against the corpus (still disclosure).
type Annotation struct {
	Site  string `json:"site"`
	Kind  string `json:"kind"`
	Note  string `json:"note"`
	By    string `json:"by,omitempty"`
	Claim string `json:"claim,omitempty"`
}

// MatchAnnotations returns the annotations in anns whose (Site, Kind) identify the
// blind spot at (site, kind). It is the single matching rule shared by every card
// that echoes annotation context (ground, impact), so they cannot drift apart.
func MatchAnnotations(anns []Annotation, site, kind string) []Annotation {
	var out []Annotation
	for _, a := range anns {
		if a.Site == site && a.Kind == kind {
			out = append(out, a)
		}
	}
	return out
}

// AnnotationLine renders one annotation as an indented disclosure line to print
// beneath the blind spot it explains: the note, and the author when recorded.
// Shared so the context reads identically on every card.
func AnnotationLine(a Annotation) string {
	line := "    🗒 " + a.Note
	if a.Claim != "" {
		line += " [claims: " + a.Claim + "]"
	}
	if a.By != "" {
		line += " — " + a.By
	}
	return line + "\n"
}

// WriteBlindSpots writes each blind spot in spots — its row via rowFmt (no trailing
// newline; this adds one) — and then, once per (Site, Kind) seam under its first row,
// that seam's annotation lines from anns. It is the single home of the "annotation
// under the seam's first row, deduped by (Site, Kind)" rule that the reach, ground, and
// impact lenses all render, so the three cannot drift (CLAUDE.md: one source of truth);
// only the per-row text, supplied by rowFmt, differs between them.
func WriteBlindSpots(w io.Writer, spots []BlindSpot, anns []Annotation, rowFmt func(BlindSpot) string) {
	shown := map[[2]string]bool{}
	for _, s := range spots {
		_, _ = fmt.Fprintln(w, rowFmt(s))
		key := [2]string{s.Site, s.Kind}
		if shown[key] {
			continue
		}
		shown[key] = true
		for _, a := range MatchAnnotations(anns, s.Site, s.Kind) {
			_, _ = io.WriteString(w, AnnotationLine(a))
		}
	}
}

// FrontierSection mirrors flowmap's disclosed frontier: the per-site markers, the
// aggregate count of routes whose severance could not be confirmed (so a consumer
// cannot misread a 0 attribution loss as a proof of no severance), and the coverage
// caveat naming what the attribution signal confirms.
type FrontierSection struct {
	Markers           []FrontierMarker `json:"markers,omitempty"`
	UnconfirmedRoutes int              `json:"unconfirmed_routes,omitempty"`
	Coverage          string           `json:"coverage,omitempty"`
}

// FrontierMarker is one classified frontier site, mirroring flowmap's frontier
// wire shape. Bin is the open taxonomy vocabulary ("A"/"B"/"B2"/"C"); a consumer
// MUST treat an unrecognized bin as "disclosed but unclassified" rather than
// assuming a meaning — the fail-closed convention for every graph-carried enum.
type FrontierMarker struct {
	Kind          string `json:"kind"`
	Bin           string `json:"bin"`
	Site          string `json:"site"`
	Owner         string `json:"owner,omitempty"`
	ReclaimerHint string `json:"reclaimer_hint,omitempty"`
}

// Entrypoint is one named root flowmap discovered: an HTTP route or a consumed
// topic, joined to its handler function. Names are registration-site literals
// (a stdlib root may lack a method; a mounted route carries only its leaf
// pattern), so consumers match them segment-wise, never exactly-or-nothing.
type Entrypoint struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Fn   string `json:"fn"`
}

// EffectOrderFact is one partial-effect order fact flowmap computed from a
// function's CFG: the named committed effect can execute before the named
// fallible call on some path (Always: on every path reaching it). Triage reads
// these to answer "if this call faults, what may already be committed?" —
// possibly-committed when Always is false, certainly-committed when true.
type EffectOrderFact struct {
	Fn         string `json:"fn"`
	Effect     string `json:"effect"`
	EffectSite string `json:"effect_site"`
	Callee     string `json:"callee"`
	CalleeSite string `json:"callee_site"`
	Always     bool   `json:"always,omitempty"`
	// Via names the ALWAYS-effect callee when the effect is committed inside
	// a call one frame down (a derived site, CX-3) — provenance for the
	// fault card, never part of fact identity.
	Via string `json:"via,omitempty"`
}

// Obligation is one path-obligation verdict flowmap computed from a function's
// SSA CFG against a .flowmap.yaml rule. groundwork only judges it: VIOLATED is
// a gate-failing finding, CANT-PROVE and UNMATCHED are disclosed abstentions,
// SATISFIED is the proof and produces no finding. Identity is (rule, fn, site);
// detail is presentation only.
//
// Status is an open vocabulary across the trust boundary: flowmap and
// groundwork decode this section independently and on purpose, so the judge
// MUST fail closed on a status it does not recognize (surface a caution,
// never fall through) — the convention for every graph-carried enum.
type Obligation struct {
	Rule   string `json:"rule"`
	Kind   string `json:"kind"`
	Fn     string `json:"fn,omitempty"`
	Site   string `json:"site,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Node is one first-party function.
type Node struct {
	FQN  string `json:"fqn"`
	Sig  string `json:"sig"`
	Tier int    `json:"tier"`
	// Package is the producer's typed import path for the node's defining package
	// (empty for a synthetic node, and for a graph built before the field existed).
	// Decoded on this side of the trust boundary like every other field —
	// DisallowUnknownFields would reject it otherwise. Disclosure-only: no verdict,
	// count, layer assignment, or reachability computation reads it.
	Package  string `json:"package,omitempty"`
	Fallible bool   `json:"fallible,omitempty"`
	// File / Line / EndLine locate the node's declaration in source: File is the
	// producer's defining file relative to the service dir, Line/EndLine the 1-based
	// span of the `func` declaration (empty/zero for a synthetic node, and for a graph
	// built before the fields existed). Decoded on this side of the trust boundary like
	// every other field — DisallowUnknownFields would reject them otherwise. They let a
	// caller intersect a git diff against each node's span to recover the author-edited
	// FQN set `review-triage --scope-fqns` consumes. Disclosure-only: no verdict, count,
	// layer assignment, or reachability computation reads them.
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
}

// Edge is a call from a first-party function (From, always a Node) to another
// first-party function or to a typed boundary sink (To). Boundary names the
// external-effect kind for boundary edges (outbound-sync, outbound-async,
// inbound); it is empty for internal function-to-function edges.
type Edge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Tier       int    `json:"tier"`
	Boundary   string `json:"boundary,omitempty"`
	Concurrent bool   `json:"concurrent,omitempty"`

	// Via names the reclaimer that recovered this edge (flowmap's opt-in
	// `--reclaim`), empty for a base call-graph edge. A reclaimed edge is one
	// real execution can take that the builder lost at a framework dispatch seam
	// (the oapi strict-server wrapper→closure hop) — reclaimers only ADD sound
	// can-reach edges (R2), never remove one. groundwork decodes it on its own
	// side of the trust boundary like every other graph-carried field: a
	// reclaimed graph must be CONSUMABLE (init/fitness/verify/review/reach), and
	// a verdict computed over it self-discloses that it leaned on a reclaimed
	// substrate via ReclaimCaveat, the Algo/Caveats discipline extended.
	Via string `json:"via,omitempty"`
}

// BlindSpot is one disclosed gap in the graph's knowledge. Site is a first-party
// FQN (reflect, HighFanOut) or a package path (unsafe, cgo, go:linkname). The
// graph view carries only the graph-completeness subset; the boundary subset
// (dynamically-named publish/dispatch) rides the boundary contract instead, and
// surfaces in the graph as a <dynamic> edge target.
type BlindSpot struct {
	Kind   string `json:"kind"`
	Site   string `json:"site"`
	Detail string `json:"detail"`
	// Severity is the producer's signal/noise tier ("effect-bearing" vs "trivial"):
	// carried by every ExternalBoundaryCall, and by the benign subset of the func()
	// dispatch channel (a context.CancelFunc UnresolvedCall/ConcurrentDispatch is tagged
	// "trivial"); empty for every other kind, for an unclassified func() seam, and for a
	// graph built before the tier existed. Decoded on this side of the trust boundary
	// like every other field — DisallowUnknownFields would reject it otherwise — and
	// surfaced beside the spot so a reader separates the effect-bearing seams from the
	// stdlib/framework noise. Disclosure-only: no verdict, count, or reachability
	// computation reads it (§21.A).
	Severity string `json:"severity,omitempty"`
	// Package is the third-party package an ExternalBoundaryCall hands off to (empty for
	// other kinds). Decoded on this side of the trust boundary like every other field so
	// the strict reader accepts it; disclosure-only.
	Package string `json:"package,omitempty"`
}

// DedupKey is the identity of a blind spot for set operations: the (Kind, Site, Detail)
// tuple. It is the ONE source for "what makes two blind spots the same", shared by
// impact's gather-and-dedup and reviewtriage's base-vs-branch new-vs-carried diff so the
// two can never drift on seam identity (CLAUDE.md one-source-of-truth) — a drift there
// would silently misclassify new blindness as carried.
func (b BlindSpot) DedupKey() string { return b.Kind + "\x00" + b.Site + "\x00" + b.Detail }

// externalBoundaryKind is the wire Kind value of an ExternalBoundaryCall blind spot.
// A local const (not an import of the producer's blindspots package) keeps groundwork
// on its own side of the trust boundary — the convention every graph-carried enum
// here follows; the JSON contract is pinned by the committed goldens.
const externalBoundaryKind = "ExternalBoundaryCall"

// EBCTierNote summarizes the ExternalBoundaryCall signal/noise split inside a
// blind-spot set as a parenthetical (" (2 effect-bearing, 7 trivial external)"), or
// "" when the set holds no ExternalBoundaryCall. It makes a bare blind-spot COUNT
// readable: most of an EBC-heavy count is framework/utility plumbing, not the
// effect-bearing seams a reviewer acts on (§21.A). Disclosure-only — it reorders
// attention, never a verdict or the count itself. Tiers print in a fixed order so the
// note is deterministic; an EBC with no severity (a pre-tier graph) is "unclassified".
func EBCTierNote(spots []BlindSpot) string {
	var effect, trivial, unclassified int
	for _, s := range spots {
		if s.Kind != externalBoundaryKind {
			continue
		}
		switch s.Severity {
		case "effect-bearing":
			effect++
		case "trivial":
			trivial++
		default:
			unclassified++
		}
	}
	if effect+trivial+unclassified == 0 {
		return ""
	}
	var parts []string
	if effect > 0 {
		parts = append(parts, fmt.Sprintf("%d effect-bearing", effect))
	}
	if trivial > 0 {
		parts = append(parts, fmt.Sprintf("%d trivial", trivial))
	}
	if unclassified > 0 {
		parts = append(parts, fmt.Sprintf("%d unclassified", unclassified))
	}
	return " (" + strings.Join(parts, ", ") + " external)"
}

// ProvenanceLine renders the one-line call-graph substrate disclosure shared by
// every groundwork surface that echoes provenance (fitness, review, verify), so
// they word it identically. An empty algo means the graph predates provenance
// recording — stated as "unrecorded", never implying a substrate. caveats are
// the recorded soundness/precision notes, joined — and they are disclosed even when
// the substrate itself is unrecorded: a reclaim note or the committed-corpus
// code-identity disclosure is a trust assumption the verdict leaned on regardless of
// whether the call-graph algo was recorded, so an algo-less graph must NOT silently
// swallow it (the prime-directive no-silent-drop these disclosures exist to honor).
func ProvenanceLine(algo string, caveats []string) string {
	line := "substrate: " + algo
	if algo == "" {
		line = "substrate: unrecorded (graph predates provenance; regenerate with current flowmap)"
	}
	if len(caveats) > 0 {
		line += " — " + strings.Join(caveats, "; ")
	}
	return line + "\n"
}

// ReclaimCaveat returns a substrate caveat disclosing that this graph carries
// reclaimed edges (flowmap's opt-in `--reclaim` / `--reclaim-middleware`), or "" when it
// carries none. It names each reclaimer and its edge count so a verdict computed over the graph is
// AUDITABLE as reclaim-informed — the Algo/Caveats substrate discipline (R3)
// extended to the reclaimer provenance. Folded into the caveats every verdict
// surface already echoes (fitness/verify/review), so a reader sees on the same
// substrate line that the verdict leaned on edges recovered at a dispatch seam.
//
// A proof of ABSENCE over a reclaimed graph is at least as strong as over the base
// graph (reclaimers only add sound edges — R2 — so they can turn provenAbsent→
// reachable, never the reverse); the disclosure exists so a REACHABLE verdict that
// rests on a reclaimed edge is not mistaken for one the base graph already saw.
func (g *Graph) ReclaimCaveat() string {
	counts := map[string]int{}
	for _, e := range g.Edges {
		// Only EDGE reclaimers (added call edges at a dispatch seam) belong on this
		// line. The SQL label fold also carries a Via, but on a BOUNDARY edge — it
		// recovers a verb, not an edge — so it is disclosed separately
		// (SQLFoldCaveat) to keep the two reclaimer KINDS independently auditable.
		if e.Via != "" && !e.IsBoundary() {
			counts[e.Via]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	vias := setutil.SortedKeys(counts)
	parts := make([]string, 0, len(vias))
	for _, v := range vias {
		parts = append(parts, fmt.Sprintf("%d via %s", counts[v], v))
	}
	return "reclaim-informed: " + strings.Join(parts, ", ") +
		" edge(s) recovered at a dispatch seam (flowmap --reclaim*) — a reachable verdict may rest on a reclaimed edge"
}

// SQLFoldCaveat returns a substrate caveat disclosing that this graph carries DB
// effect labels whose verb the SQL const-accumulation fold recovered (flowmap's
// opt-in `--reclaim-sql`), or "" when it carries none. A folded label feeds the
// write-surface classification (a recovered mutating verb is charged to the
// budget; a recovered SELECT is trusted as a read), so a verdict that leaned on
// one must disclose it — the label analogue of ReclaimCaveat (plan §3, L3).
//
// The fold is sound-or-abstain, so a folded label is at least as trustworthy as a
// call-site constant; the disclosure exists so a verdict resting on a RECOVERED
// verb is auditable as such, not mistaken for one the labeler read directly.
func (g *Graph) SQLFoldCaveat() string {
	n := 0
	for _, e := range g.Edges {
		if e.Via != "" && e.IsBoundary() {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("sql-fold-informed: %d DB effect verb(s) recovered from constant-fragment SQL "+
		"(flowmap --reclaim-sql) — a write/read classification may rest on a folded verb", n)
}

// SubstrateMismatchCaveat returns a disclosure when a policy proposed on
// policyAlgo is being checked against a graph built on graphAlgo and the two
// differ, or "" when there is nothing to flag (either is unrecorded, or they
// agree). The algorithms are all sound, so a proof of absence holds on any of
// them; they differ in PRECISION, so a coarser graph (rta over-approximates
// dynamic dispatch) can show a reachability finding a refined one (vta) ruled
// out — the field footgun where a vta-proposed policy swept with the rta default
// produced spurious must_not_reach violations. Naming the mismatch lets a reader
// treat such a finding as an analyzer artifact rather than a regression. Shared by
// fitness (as a Caution) and the review gate (as a substrate caveat) so the two
// surfaces word it identically — the Algo/Caveats provenance discipline (R3).
func SubstrateMismatchCaveat(policyAlgo, graphAlgo string) string {
	if policyAlgo == "" || graphAlgo == "" || policyAlgo == graphAlgo {
		return ""
	}
	return fmt.Sprintf("substrate mismatch: policy proposed on %s, graph built on %s — the algorithms differ in precision, so a reachability finding may be an analyzer artifact, not a regression; build the gate graph with `flowmap graph --algo %s`, or re-init the policy on this graph", policyAlgo, graphAlgo, policyAlgo)
}

// AlgoMismatchCaveat returns a disclosure when the base and branch graphs were
// built on different call-graph algorithms, or "" when there is nothing to flag
// (either side is unrecorded, or they agree). The algorithms are all sound but
// differ in precision, so a delta computed across substrates can move for the
// analyzer's reasons, not the code's (R3). It is the base↔branch sibling of
// SubstrateMismatchCaveat (policy substrate vs graph) and ToolMismatchCaveat
// (producer vs producer) — all three are helpers so review and verify word the
// provenance-mismatch family identically (one source of truth), rather than one
// living inline at the call site.
func AlgoMismatchCaveat(baseAlgo, branchAlgo string) string {
	if baseAlgo == "" || branchAlgo == "" || baseAlgo == branchAlgo {
		return ""
	}
	return fmt.Sprintf("base graph built on %s, branch on %s — substrate differs; a delta may be the analyzer's, not the code's", baseAlgo, branchAlgo)
}

// ToolMismatchCaveat returns a disclosure when the base and branch graphs were
// produced by two different flowmap builds, or "" when there is nothing to flag
// (either side is unrecorded, or they agree). flowmap's "same code → same graph"
// determinism holds only WITHIN one tool version: a base built by build A and a
// branch by build B — same code, same stamp, same algo — can still diff on a pure
// tool artifact (a relabeled effect, an SSA-order shift). Naming the producer skew
// lets a reader treat such a delta as a tool artifact, not a code change (R11). It
// is the producer dimension of the Stamp/Algo provenance family — the code identity
// is bound at the gate via --expect and the base↔branch algo via AlgoMismatchCaveat
// (with SubstrateMismatchCaveat guarding the policy-vs-graph algo); this closes the
// last comparable-inputs dimension. Shared so review and verify word it identically
// (one source of truth).
func ToolMismatchCaveat(baseTool, branchTool string) string {
	if baseTool == "" || branchTool == "" || baseTool == branchTool {
		return ""
	}
	return fmt.Sprintf("producer mismatch: base graph built by flowmap %s, branch by %s — graphs were built by different tool versions, so a diff may be a tool artifact, not a code change; rebuild both sides with one flowmap build", baseTool, branchTool)
}

// IsBoundary reports whether the edge targets an external sink rather than a
// first-party function.
func (e Edge) IsBoundary() bool { return strings.HasPrefix(e.To, boundaryPrefix) }

// IsDynamic reports whether the edge targets a boundary effect the graph could
// not name statically — a soundness frontier for any reachability claim through
// it.
func (e Edge) IsDynamic() bool { return strings.Contains(e.To, dynamicMarker) }

// Load decodes a graph from JSON. It rejects unknown fields so a flowmap schema
// change that groundwork has not been taught about fails loudly here rather than
// being silently dropped.
func Load(r io.Reader) (*Graph, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var g Graph
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("groundwork/graph: decode: %w", err)
	}
	// A trusted graph file is exactly one JSON value. Anything after the decoded
	// object — a second concatenated document, stray bytes, or an extra closing
	// delimiter — means the input is not the single graph it claims to be, so
	// refuse rather than silently gate on only the first value (tenet 2, fail
	// closed). dec.More() alone is insufficient: it returns false when the trailing
	// bytes begin with '}' or ']', so a stray delimiter would slip through. Reading
	// the next token instead must reach clean EOF; any token — or a parse error —
	// is trailing data. Insignificant trailing whitespace/newline is skipped by the
	// tokenizer and still reaches io.EOF, so a normal file with a trailing newline
	// loads fine.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("groundwork/graph: trailing data after graph JSON (expected a single graph object)")
	}
	if g.Nodes == nil {
		return nil, fmt.Errorf("groundwork/graph: missing nodes (not a flowmap graph?)")
	}
	// Fail-closed decoder invariant: an edge's boundary-ness must agree between the
	// two ways the codebase asks it — the To-prefix (IsBoundary(), what evalReach
	// and every walk key on) and the Boundary field. A graph with a boundary: To
	// but an empty Boundary field (or vice versa) is malformed; accepting it lets
	// the two predicates disagree downstream and mask a real reachable violation as
	// an "unbindable target" caution (H-5). Refuse here rather than gate on it.
	for i, e := range g.Edges {
		if e.IsBoundary() != (e.Boundary != "") {
			return nil, fmt.Errorf("groundwork/graph: edge %d (%s -> %s) has inconsistent boundary marking: To-prefix=%t but Boundary field=%q", i, e.From, e.To, e.IsBoundary(), e.Boundary)
		}
	}
	return &g, nil
}

// LoadFile reads and decodes a graph from a file path.
func LoadFile(path string) (*Graph, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	g, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return g, nil
}
