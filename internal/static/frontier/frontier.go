// Package frontier classifies a built call graph's FRONTIER — every place static
// reachability stops being able to answer — into the taxonomy from
// docs/design/frontier-instrumentation-plan.md, deterministically and with no
// coupling to any verdict surface (rule R3: a frontier label can only
// (mis)prioritize our own work, never change a fitness/verify result).
//
// It is intentionally free of any serialization type: it takes a minimal Input,
// not a graphio.Graph, so the analysis does not depend on the producer's schema —
// graphio imports frontier (to embed the section), never the reverse.
//
// The four bins:
//
//	A  — truly dynamic: resolved only at runtime (<dynamic> bus/HTTP targets,
//	     reflection, cgo/unsafe/linkname). Irreducible statically; disclose.
//	B  — reclaimable structure: statically determined AND reconnectable by a known
//	     reclaimer (the strict-server `$1` dispatch seam; a route whose cone is
//	     starved of every effect). The static lever — a sound reclaimer ADDS the
//	     missing edge. A severed closure no reclaimer recognizes (an errgroup or
//	     constructor closure dispatched through a struct field) is NOT B: it is
//	     disclosed as A, so the frontier never promises a reclaim `--reclaim` cannot
//	     perform (§21.②).
//	B2 — consumer-reclaimable: opaque only because the SOURCE is non-constant (a
//	     `db ExecContext` from runtime-built SQL). It splits in two (plan §2):
//	       B2a — accumulator-reclaimable by the maintainer-side SQL const-fold
//	             (--reclaim-sql): a constant statement laundered through a builder.
//	             Once folded it gains a readable verb and LEAVES the frontier, so it
//	             is visible as the drop in the opaque-db count, not a standing marker.
//	       B2b — genuinely consumer-reclaimable: a dynamic verb or a runtime
//	             identifier spliced into the text, which the fold cannot recover. In
//	             a folded graph the remaining opaque-db markers ARE the B2b residue.
//	C  — over-approximation: sound but imprecise (HighFanOut shared dispatch). Not
//	     blindness; precision.
package frontier

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// Bin is one taxonomy class. The string values are the stable wire vocabulary.
type Bin string

const (
	BinA  Bin = "A"  // truly dynamic
	BinB  Bin = "B"  // reclaimable structure
	BinB2 Bin = "B2" // consumer-reclaimable (opaque SQL)
	BinC  Bin = "C"  // over-approximation
)

// Marker is one frontier site, binned, with the reclaim target when there is one.
// It is the wire shape graphio embeds as the graph's `frontier` section.
type Marker struct {
	Kind          string `json:"kind"`                     // e.g. "severed-closure", "dynamic-bus", "opaque-db", "starved-entrypoint", "HighFanOut"
	Bin           Bin    `json:"bin"`                      //
	Site          string `json:"site"`                     // the FQN or effect label the marker is about
	Owner         string `json:"owner,omitempty"`          // the reclaim target / the function making the effect
	ReclaimerHint string `json:"reclaimer_hint,omitempty"` // what a reclaimer (B) or the consumer (B2) would do
}

// Input is the minimal graph view Classify needs: first-party node FQNs, the call
// and boundary edges, the disclosed blind spots, and the named entrypoints. It is
// deliberately free of any serialization type so the classifier stays independent
// of the producer's schema.
type Input struct {
	Nodes       []string
	Edges       []InEdge
	BlindSpots  []InBlindSpot
	Entrypoints []InEntry
	// Folded reports whether the graph was built with the SQL const-fold
	// (--reclaim-sql) applied. When true, every opaque-db marker that REMAINS is the
	// genuine B2b residue (the fold tried and abstained), so its disclosure is the
	// consumer ask; when false, the opaque-db set is the undifferentiated B2 union,
	// and the disclosure points the reader at --reclaim-sql first.
	Folded bool

	// Reclaimable is the set of closure FQNs a known dispatch-seam reclaimer (the
	// strict-server reclaimer) could ACTUALLY reconnect — computed upstream as a dry
	// run over the program (graphio supplies it; the classifier stays serialization-
	// and SSA-free). It gates the severed-closure bin: a severed closure IN this set is
	// reclaimable (B) and the marker promises the connect; one NOT in it is irreducible
	// (A) and the marker says so, so the frontier never advertises a reclaim `--reclaim`
	// cannot perform (§21.②). Empty (the classifier's standalone-test default) means no
	// closure is reclaimable, so every severed closure is disclosed as A — fail closed:
	// without proof a reclaimer can deliver, do not promise one.
	Reclaimable []string

	// MiddlewareReclaimable is the set of function FQNs whose standalone UnresolvedCall is a
	// middleware-application loop the middleware-chain reclaimer proves EMPTY — i.e. one
	// `flowmap graph --reclaim-middleware` would resolve and clear. A standalone UnresolvedCall
	// at such a site is binned B (reclaimable) instead of A (irreducible), the same way the
	// strict-server seam is predicted via Reclaimable, so the default frontier reports an oapi/
	// chi route's blind middleware loop as reclaimable rather than irreducible. Empty (the
	// standalone-test default) leaves every UnresolvedCall in A — fail closed. Like Reclaimable
	// it is computed upstream as a dry run (graphio supplies it; the classifier stays SSA-free).
	MiddlewareReclaimable []string
}

// InEdge is a call or boundary edge (To is a "boundary:..." label for an effect).
type InEdge struct{ From, To string }

// InBlindSpot is a disclosed blind spot; Kind is the blindspots.Kind string value.
type InBlindSpot struct{ Kind, Site string }

// InEntry is a named entrypoint and the function registered to handle it.
type InEntry struct{ Fn, Name string }

// Coverage states what the starvation/attribution signal CONFIRMS, framed like the
// io_budget "lower bound, not a proof" caution: a route is flagged severed only for
// the oapi-codegen strict-server shape, so attribution_loss is a LOWER BOUND — a 0
// with unconfirmed routes present is "no CONFIRMED seam", never a proof of no
// severance. It rides the disclosed frontier so a consumer reading the data (not
// just the human view) cannot misread the number.
const Coverage = "starvation confirmed for the oapi strict-server shape only; " +
	"unconfirmed routes reach no effect for an unverified reason (a no-op, or an unrecognized dispatch seam) — " +
	"attribution_loss is a LOWER BOUND, not a proof of no severance"

// Result is what Classify returns: the sorted marker list plus the UNCONFIRMED
// routes — entrypoints that reach no effect and are NOT confirmed-severed (a no-op
// route, or a seam shape this classifier does not recognize). The unconfirmed list
// is the third state of the three-valued frontier (confirmed-severed / proven-clean
// / unconfirmed): it is what stops a 0 attribution_loss from reading as a proof.
type Result struct {
	Markers           []Marker
	UnconfirmedRoutes []string // entrypoint FQNs reaching no effect, severance unconfirmed
	Folded            bool     // the SQL const-fold was applied (opaque-db markers are the B2b residue)
}

// Report is the deterministic roll-up: every marker, the per-bin counts, the two
// headline ratios — reclaimable share (B of all) and attribution loss (CONFIRMED
// severed routes / all routes) — and the unconfirmed-route population that keeps
// attribution loss honest as a lower bound.
type Report struct {
	Algo               string      `json:"algo,omitempty"` // call-graph algorithm provenance (rta|vta|cha)
	Markers            []Marker    `json:"markers"`
	Counts             map[Bin]int `json:"counts"`
	Entrypoints        int         `json:"entrypoints"`
	StarvedEntrypoints int         `json:"starved_entrypoints"` // CONFIRMED severed
	UnconfirmedRoutes  []string    `json:"unconfirmed_routes,omitempty"`
	Coverage           string      `json:"coverage,omitempty"`
	ReclaimableShare   float64     `json:"reclaimable_share"`    // B / total markers
	AttributionLoss    float64     `json:"attribution_loss"`     // CONFIRMED severed / entrypoints (a lower bound)
	SQLFolded          bool        `json:"sql_folded,omitempty"` // --reclaim-sql applied: opaque-db count is the B2b residue
}

// Classify bins in's frontier into a sorted, deduplicated marker list, plus the
// unconfirmed-route population. Pure function of the input: no clock, no corpus, no
// verdict coupling.
func Classify(in *Input) *Result {
	nodes := make(map[string]bool, len(in.Nodes))
	for _, n := range in.Nodes {
		nodes[n] = true
	}
	out := map[string][]string{}
	hasCaller := map[string]bool{}
	for _, e := range in.Edges {
		out[e.From] = append(out[e.From], e.To)
		if nodes[e.To] {
			hasCaller[e.To] = true
		}
	}

	reclaimable := make(map[string]bool, len(in.Reclaimable))
	for _, fqn := range in.Reclaimable {
		reclaimable[fqn] = true
	}
	mwReclaimable := make(map[string]bool, len(in.MiddlewareReclaimable))
	for _, fqn := range in.MiddlewareReclaimable {
		mwReclaimable[fqn] = true
	}

	seen := map[[3]string]bool{}
	var markers []Marker
	add := func(m Marker) {
		key := [3]string{m.Kind, m.Site, m.Owner}
		if seen[key] {
			return
		}
		seen[key] = true
		markers = append(markers, m)
	}

	// Effect-edge markers: dynamic targets (A) and opaque DB writes (B2).
	for _, e := range in.Edges {
		if !strings.HasPrefix(e.To, "boundary:") {
			continue
		}
		label := strings.TrimPrefix(e.To, "boundary:")
		switch {
		case strings.Contains(e.To, "<dynamic>"):
			kind := "dynamic-effect"
			if strings.HasPrefix(label, "bus ") {
				kind = "dynamic-bus"
			}
			add(Marker{Kind: kind, Bin: BinA, Site: label, Owner: e.From,
				ReclaimerHint: "runtime-resolved target — disclose; resolvable only by observation, never statically"})
		case strings.HasPrefix(label, "db ") && !readableDBVerb(label):
			add(Marker{Kind: "opaque-db", Bin: BinB2, Site: label, Owner: e.From,
				ReclaimerHint: opaqueDBHint(in.Folded, e.From)})
		}
	}

	// Structural markers: the severed `$N` dispatch seam. A closure qualifies as
	// severed only when ALL of:
	//   - it is a graph root (no caller) — the forward edge into it was lost;
	//   - its de-`$N` lexical parent IS a node — we know the exact reclaim target;
	//   - it REACHES A BOUNDARY EFFECT — it hides real downstream work.
	// The effect requirement is the soundness filter (Gate 2 of the plan): a leaf
	// callback (a sort comparator, an empty closure) is also a parentless `$N`
	// node, but it hides nothing AND a `parent → callback` edge would usually be
	// FALSE (the parent PASSES it to a higher-order function, it does not CALL it).
	// Which nodes can reach a boundary effect — computed ONCE by a reverse sweep
	// from the effect-producing nodes (O(V+E)), so the severed-closure and
	// entrypoint checks below are O(1) lookups instead of a fresh forward BFS per
	// candidate (which was O((k+m)·(V+E))).
	//
	// The bin then turns on RECLAIMABILITY (§21.②): a severed closure a known
	// reclaimer can actually reconnect (in.Reclaimable — the strict-server seam) is B,
	// and its marker promises the connect; one no reclaimer recognizes (an errgroup or
	// constructor closure dispatched through a struct field — the shape `--reclaim`
	// leaves untouched) is A — irreducible to static, so the marker says exactly that
	// instead of advertising a reclaim the flag will not deliver. Previously every
	// severed closure was B, so `frontier` listed reclaims `--reclaim` could not perform.
	reaches := effectReachers(out, nodes)

	severedParent := map[string]bool{}     // parent owns an effect-bearing severed closure (confirmed severance)
	reclaimableParent := map[string]bool{} // ... and a known reclaimer can reconnect that closure (B, not A)
	for fqn := range nodes {
		if hasCaller[fqn] {
			continue
		}
		parent, ok := closureParent(fqn)
		if !ok || !nodes[parent] {
			continue
		}
		if !reaches[fqn] {
			continue
		}
		severedParent[parent] = true
		if reclaimable[fqn] {
			reclaimableParent[parent] = true
			add(Marker{Kind: "severed-closure", Bin: BinB, Site: fqn, Owner: parent,
				ReclaimerHint: "connect " + ShortName(parent) + " to this closure across the dispatch seam"})
		} else {
			add(Marker{Kind: "severed-closure", Bin: BinA, Site: fqn, Owner: parent,
				ReclaimerHint: "no static reclaimer recognizes this dispatch seam (the closure does not flow to a known dispatcher) — irreducible without runtime observation"})
		}
	}

	// Attribution: a named entrypoint reaching no boundary effect is one of two
	// states. If it owns a severed effect-bearing closure — the effect sits in its
	// OWN `$N` closure, disconnected — it is a CONFIRMED seam (starved-entrypoint).
	// Otherwise its severance is UNCONFIRMED: a genuine no-op stub, or a seam shape
	// this classifier does not recognize. The unconfirmed routes are NOT markers (they
	// would cry wolf on every health endpoint and churn the committed graph under
	// refactoring); they are returned as an aggregate so a 0 attribution_loss cannot be
	// misread as a proof of no severance.
	//
	// The starved-entrypoint is a CONFIRMED severance regardless of reclaimability —
	// so it always counts toward attribution_loss (Summarize keys on the kind, not the
	// bin). Its BIN tracks whether the owning closure is reclaimable (§21.②): B when a
	// reclaimer can close the seam (the strict-server shape), A when none can — so the
	// route is honestly disclosed as severed either way, but reclaimable-share counts
	// only the seams that are genuinely reclaimable.
	var unconfirmed []string
	for _, ep := range in.Entrypoints {
		if reaches[ep.Fn] {
			continue
		}
		if severedParent[ep.Fn] {
			bin, hint := BinA, "route reaches no effect directly; its own severed closure does, but no static reclaimer recognizes the seam — irreducible without runtime observation"
			if reclaimableParent[ep.Fn] {
				bin, hint = BinB, "route reaches no effect directly, but its own severed closure does — handler chain cut at the dispatch seam"
			}
			add(Marker{Kind: "starved-entrypoint", Bin: bin, Site: ep.Fn, Owner: ep.Name, ReclaimerHint: hint})
		} else {
			unconfirmed = append(unconfirmed, ep.Fn)
		}
	}

	// Disclosed blind spots, binned by kind. A ratified ImpeachmentSeam is
	// EXCLUDED from the frontier markers: it is not a reclaimable (or even a
	// newly-discovered) frontier but a human-ratified disclosure already enacted in
	// config, so counting it would let every ratification churn ReclaimableShare and
	// the bin ratios — a metric drifting on governance actions, not on code
	// dynamism. Its kind is still mapped by blindSpotBin (kept whole for the
	// exhaustiveness guard); it just does not enter the marker set.
	// Sites already represented by a structural or effect marker. An UnresolvedCall
	// is the low-level CAUSE (a zero-resolution func-value call) of a seam, and when
	// it sits at a dispatch seam this classifier already counted — a strictsvc wrapper
	// is a starved-entrypoint at the SAME FQN — re-emitting it would double-count and
	// drift the bin ratios. So it is deduped against, but ONLY against, a coinciding
	// marker; an UnresolvedCall at a site with no structural marker (a func-value call
	// outside any recognized dispatch seam) is a standalone blind frontier and IS
	// emitted, so the section stays complete. (Built from the markers added above —
	// structural and effect — which are all that precede this loop.)
	markedSites := map[string]bool{}
	for _, m := range markers {
		markedSites[m.Site] = true
	}
	for _, bs := range in.BlindSpots {
		if blindspots.Kind(bs.Kind).Ratified() {
			continue
		}
		// A disclosure-only kind (ExternalBoundaryCall) is a dependency-surface
		// disclosure, not a severance frontier: high-volume and a KNOWN intentional
		// boundary, not a cut in first-party reachability. Admitting it would swamp
		// ReclaimableShare — the metric of how reclaimable the SEVERANCE frontier is —
		// with framework/SDK plumbing, so it rides the blind-spots manifest and the
		// render's blind channel instead. The same predicate gates the reach frontier
		// (fitness.firstReachBlinding); blindSpotBin still maps it to A for the
		// exhaustiveness guard, but this loop never asks.
		if blindspots.Kind(bs.Kind).IsDisclosureOnlyFrontier() {
			continue
		}
		// A ratified ImpeachmentSeam never reaches here (skipped above). An
		// UnresolvedCall — or its goroutine sibling ConcurrentDispatch — that
		// coincides with a structural marker is the low-level CAUSE of that seam and
		// is redundant, so drop it; otherwise it is disclosed in its own right
		// (blindSpotBin → BinA: a func-value call no structural reclaimer recognized
		// is irreducible).
		if (bs.Kind == string(blindspots.UnresolvedCall) || bs.Kind == string(blindspots.ConcurrentDispatch)) && markedSites[bs.Site] {
			continue
		}
		bin, _ := blindSpotBin(bs.Kind)
		// A standalone UnresolvedCall the middleware-chain reclaimer proves EMPTY is
		// reconnectable by --reclaim-middleware (which clears it), so it is B, not the
		// irreducible A blindSpotBin defaults to — the same prediction the strict-server
		// seam gets via Reclaimable. The non-empty/dynamic middleware seam is NOT in this set
		// (it stays A: its per-middleware next.ServeHTTP hop is a genuine residual).
		if bs.Kind == string(blindspots.UnresolvedCall) && mwReclaimable[bs.Site] {
			bin = BinB
		}
		add(Marker{Kind: bs.Kind, Bin: bin, Site: bs.Site})
	}

	sort.Slice(markers, func(i, j int) bool {
		a, b := markers[i], markers[j]
		if a.Bin != b.Bin {
			return a.Bin < b.Bin
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Site != b.Site {
			return a.Site < b.Site
		}
		return a.Owner < b.Owner
	})
	sort.Strings(unconfirmed)
	return &Result{Markers: markers, UnconfirmedRoutes: unconfirmed, Folded: in.Folded}
}

// opaqueDBHint is the B2 disclosure for an opaque DB effect, split by whether the
// SQL const-fold already ran (plan §2). Folded: the site is the genuine B2b
// residue — the fold tried and could not recover a verb (a dynamic verb, or a
// runtime identifier spliced into the statement text), so the consumer must make
// the statement constant. Unfolded: it is the undifferentiated B2 union, so the
// reader is pointed at --reclaim-sql first — it folds the constant-fragment-builder
// sub-class (B2a) for free, leaving only the residue to hoist to a const.
func opaqueDBHint(folded bool, owner string) string {
	if folded {
		return "B2b genuine residue: --reclaim-sql could not recover a verb (dynamic verb, or a runtime identifier spliced into the SQL text) — make the statement a constant (" + ShortName(owner) + ")"
	}
	return "opaque SQL built at runtime — run --reclaim-sql to fold the constant-fragment-builder sub-class (B2a); make any residue a constant (" + ShortName(owner) + ")"
}

// Summarize rolls a Result up into the report ratios. entrypoints is the total
// number of named routes (needed for attribution loss; not derivable from the
// markers alone). It carries the unconfirmed-route list and the Coverage caveat so
// the report — the machine-readable --json view — discloses the lower-bound nature
// of attribution_loss, not just the human text.
func Summarize(r *Result, entrypoints int) *Report {
	counts := map[Bin]int{BinA: 0, BinB: 0, BinB2: 0, BinC: 0}
	starved := 0
	for _, m := range r.Markers {
		counts[m.Bin]++
		if m.Kind == "starved-entrypoint" {
			starved++
		}
	}
	rep := &Report{
		Markers:            r.Markers,
		Counts:             counts,
		Entrypoints:        entrypoints,
		StarvedEntrypoints: starved,
		UnconfirmedRoutes:  r.UnconfirmedRoutes,
		Coverage:           Coverage,
		SQLFolded:          r.Folded,
	}
	if len(r.Markers) > 0 {
		rep.ReclaimableShare = float64(counts[BinB]) / float64(len(r.Markers))
	}
	if entrypoints > 0 {
		rep.AttributionLoss = float64(starved) / float64(entrypoints)
	}
	return rep
}

// effectReachers returns the set of nodes that can reach at least one boundary
// effect over first-party call edges. It is computed once per classification by a
// reverse sweep: seed with every node that has a boundary out-edge, then propagate
// backward along caller→callee edges. A node is in the set iff it, or something it
// transitively calls, makes a boundary effect — the same predicate the old
// per-seed forward BFS computed, in one O(V+E) pass instead of one per candidate.
// The result is a set, so the sweep order does not affect it (determinism holds).
func effectReachers(out map[string][]string, nodes map[string]bool) map[string]bool {
	rev := map[string][]string{} // callee -> first-party callers
	reaches := map[string]bool{}
	var queue []string
	for from, tos := range out {
		for _, to := range tos {
			if strings.HasPrefix(to, "boundary:") {
				if !reaches[from] {
					reaches[from] = true
					queue = append(queue, from)
				}
			} else if nodes[to] {
				rev[to] = append(rev[to], from)
			}
		}
	}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, caller := range rev[cur] {
			if !reaches[caller] {
				reaches[caller] = true
				queue = append(queue, caller)
			}
		}
	}
	return reaches
}

// closureParent returns the FQN with a trailing `$N` stripped — the lexical parent
// a generated closure was defined in — and whether fqn was a closure at all.
func closureParent(fqn string) (string, bool) {
	i := strings.LastIndex(fqn, "$")
	if i < 0 {
		return "", false
	}
	suffix := fqn[i+1:]
	if !allDigits(suffix) {
		return "", false
	}
	return fqn[:i], true
}

// readableDBVerb reports whether a "db ..." label's leading token is an uppercase
// SQL keyword (SELECT/INSERT/DELETE/...) the labeler read from constant SQL, as
// opposed to a method-name fallback ("ExecContext", "call") it emits when the SQL
// is non-constant. Upper-case-and-alphabetic is the discriminator: sqlOpTable
// upper-cases the verb, while the *ssa.Function fallback name never is.
func readableDBVerb(label string) bool {
	verb := strings.TrimPrefix(label, "db ")
	if sp := strings.IndexByte(verb, ' '); sp >= 0 {
		verb = verb[:sp]
	}
	if verb == "" {
		return false
	}
	for _, r := range verb {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// blindSpotBin maps a blind-spot kind to its taxonomy bin. The second result is
// false for a kind this classifier does not yet recognize: it is disclosed as A
// (irreducible) conservatively, but TestBlindSpotBinCoversAllKinds iterates
// blindspots.Kinds() and fails when a new kind is added without an explicit
// decision here — so a kind that is actually reclaimable cannot SILENTLY land in A
// (the fail-open the catch-all default would otherwise cause).
func blindSpotBin(kind string) (Bin, bool) {
	switch kind {
	case string(blindspots.HighFanOut):
		return BinC, true // over-approximation, not blindness
	case string(blindspots.Reflect), string(blindspots.Unsafe),
		string(blindspots.Cgo), string(blindspots.Linkname),
		string(blindspots.UnresolvedDispatch), string(blindspots.NonConstantBoundaryArg),
		string(blindspots.ImpeachmentSeam), string(blindspots.UnresolvedCall),
		string(blindspots.ConcurrentDispatch), string(blindspots.ExternalBoundaryCall):
		// UnresolvedCall and its goroutine sibling ConcurrentDispatch land in A
		// whenever the marker loop emits them (a func-value call at a site no
		// structural marker covers — irreducible to static, like reflect; a `go`
		// dispatch is irreducible AND async); at a site a structural seam already
		// covers, the loop dedups them instead, so this bin applies only to the
		// standalone case. ExternalBoundaryCall is mapped here ONLY to satisfy the
		// exhaustiveness guard — the marker loop skips every Kind.IsDisclosureOnlyFrontier
		// kind, so this branch is never reached for it.
		return BinA, true // runtime/irreducible frontier (a ratified seam is irreducible to static)
	default:
		return BinA, false // unrecognized — disclosed as A, but the guard test flags it
	}
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// ShortName renders an FQN compactly (drops the module path prefix and SSA
// pointer/paren noise). It is the one FQN-shortener the producer-side views share —
// this package's frontier hints/render and the call-graph flowchart in graphio — so
// the human-facing label convention is not copied per package (CLAUDE.md: one
// source of truth). Lossy by design; the labels it feeds are backed by collision-
// free ids, so distinct deep-package functions of the same name collapsing is fine.
func ShortName(fqn string) string {
	s := strings.TrimPrefix(fqn, "(")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, ")", ""), "*", "")
}
