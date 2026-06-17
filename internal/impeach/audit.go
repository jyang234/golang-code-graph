package impeach

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/ir"
)

// VerdictCandidate is the only verdict Phase 0 emits: a behaviorally-observed
// effect on a statically-non-reachable path, BEFORE the downgrade ladder (§4)
// rules out the benign explanations. A candidate is not yet an impeachment — it
// is disclosure-only, the raw left edge of the join. Phase 1 classifies it.
const VerdictCandidate = "CANDIDATE"

// Reachability values for a candidate's static Claim, the coarse Phase-0 read of
// "why does static not reach this?" Refined by the ladder later; never asserts
// "blind" as a candidate (a blind cell abstains and is excluded, fail-closed).
const (
	// ReachUnreachable: the graph DOES model an emitter of this effect, but none
	// of its emitters is reachable from an entrypoint — a severed named effect.
	ReachUnreachable = "unreachable"
	// ReachAbsent: the graph models NO emitter of this effect at all, and no
	// disclosed blind spot (dynamic bus / opaque SQL) could cover it — a missing
	// emitter or missing root.
	ReachAbsent = "absent"
)

// Report is the deterministic, digestable artifact of one observed×static join
// over a (stamped graph, canonical trace corpus) pair — the witness schema of
// plan §5, scoped to Phase 0 (the spine; no ladder, no severance, no repair).
// Disclosure-only: it records, it never mutates the graph or moves a verdict.
type Report struct {
	Service string `json:"service"`

	// Provenance of the DENOMINATOR (the graph) — mirrors the graph header (R11).
	GraphStamp string `json:"graph_stamp,omitempty"`
	GraphTool  string `json:"graph_tool,omitempty"`
	GraphAlgo  string `json:"graph_algo,omitempty"`

	// CorpusDigest pins the exact canonical trace set audited (the NUMERATOR).
	CorpusDigest string `json:"corpus_digest"`

	Caveats []string `json:"caveats,omitempty"`

	// Candidates is the impeachment direction: observed × not-statically-reachable
	// (sorted, intrinsic order). CoverageGaps is the other direction folded in
	// (§10): statically-reachable × not-observed — the green-calibrating set, the
	// inverse of coverage.Delta computed natively in this join's key space.
	Candidates   []Witness `json:"candidates"`
	CoverageGaps []string  `json:"coverage_gaps,omitempty"`

	Digest string `json:"digest"`
}

// Witness is one candidate: an effect observed where static did not place it.
type Witness struct {
	Effect   string      `json:"effect"`   // the canonical join key (the §7 key space)
	Claim    Claim       `json:"claim"`    // the static negative under test
	Observed Observation `json:"observed"` // the runtime counterexample
	Verdict  string      `json:"verdict"`  // VerdictCandidate at Phase 0
}

// Claim is the static side of the contradiction.
type Claim struct {
	Reachability string `json:"reachability"` // ReachUnreachable | ReachAbsent
}

// Observation is the behavioral side. Entry is the coarse L0 anchor (the
// observed trace root's op); Op is the raw observed op key, which carries the
// behavioral enrichment the system-agnostic Effect key drops (e.g. the DB
// system in "DB postgres DELETE ledger").
type Observation struct {
	Flow    string `json:"flow"`
	Service string `json:"service,omitempty"`
	Entry   string `json:"entry,omitempty"`
	Op      string `json:"op"`
}

// Audit joins a stamped graph against a canonical trace corpus and returns the
// Phase-0 witness report. It is a pure function of (ix, traces): all ordering is
// intrinsic (effect key, flow, op), no map iteration or arrival order reaches the
// output, so the report — and its digest — is byte-identical across runs.
//
// Scope (disclosed in Caveats, not hidden): the join covers bus and DB boundary
// effects, the two kinds with a parity-proven label reconciliation (DBEffectKey,
// the bus op key). Outbound HTTP/RPC is not yet joined — keying it needs the same
// peer-dropping reconciliation DBEffectKey does for the DB system, deferred so
// Phase 0 never guesses a key. A dynamic-bus or opaque-SQL blind spot in the
// graph COVERS an unnamed observed effect (static abstains there — the
// RECLAIMED-LIVE cell), so such an effect is excluded from candidates rather than
// laundered into a false "absent" negative (tenet 4: a negative holds only
// outside the disclosed blind spots).
func Audit(service string, ix *graph.Index, traces []*ir.CanonicalTrace) Report {
	r := Report{
		Service:      service,
		GraphStamp:   ix.Stamp(),
		GraphTool:    ix.Tool(),
		GraphAlgo:    ix.Algo(),
		CorpusDigest: corpusDigest(traces),
		Candidates:   []Witness{},
	}

	named, reachable, blind, busDynamic, dbUnreadable := staticEffectSets(ix)
	observed := observedEffects(traces)

	var caveats []string
	if busDynamic > 0 {
		caveats = append(caveats, plural(busDynamic, "dynamically-named bus effect")+" in the graph: an unnamed observed publish/consume is treated as covered by these (reclaimed-live), never impeached")
	}
	if dbUnreadable > 0 {
		caveats = append(caveats, plural(dbUnreadable, "opaque DB effect")+" in the graph: an unnamed observed DB op is treated as covered by these (reclaimed-live), never impeached")
	}
	caveats = append(caveats, "Phase 0 joins caused effects only: bus PUBLISH and DB. Inbound CONSUME/HTTP-server spans are entries, not effects; outbound HTTP/RPC is deferred (label parity)")

	blindCovered := 0
	for _, o := range observed {
		switch {
		case reachable[o.key]:
			// CONFIRMED-LIVE — static proves a path, behavior agrees.
		case blind[o.key]:
			// Reachable only across a disclosed frontier seam / blind spot:
			// static abstains here (RECLAIMED-LIVE), so behavior fills a known
			// blind spot rather than impeaching a clean negative.
			blindCovered++
		case !named[o.key]:
			// Static names no emitter at all. A disclosed dynamic-bus / opaque-SQL
			// blind spot of the matching family covers it ⇒ static abstains ⇒ not
			// a candidate; otherwise the disclosure was incomplete ⇒ ABSENT.
			if (isBusKey(o.key) && busDynamic > 0) || (isDBKey(o.key) && dbUnreadable > 0) {
				blindCovered++
				continue
			}
			r.Candidates = append(r.Candidates, witness(o, ReachAbsent))
		default:
			// Named, with an emitter, but unreachable even allowing every
			// disclosed seam — a genuine severed effect static failed to disclose.
			r.Candidates = append(r.Candidates, witness(o, ReachUnreachable))
		}
	}
	if blindCovered > 0 {
		caveats = append(caveats, plural(blindCovered, "observed effect")+" excluded from candidates as blind-spot/frontier-covered (RECLAIMED-LIVE)")
	}

	// CoverageGaps: statically-reachable effects no trace exercised (the green
	// scoped to its evidence, §2). Inverse of the impeachment direction.
	obsKeys := map[string]bool{}
	for _, o := range observed {
		obsKeys[o.key] = true
	}
	var gaps []string
	for k := range reachable {
		if !obsKeys[k] {
			gaps = append(gaps, k)
		}
	}
	sort.Strings(gaps)
	r.CoverageGaps = gaps

	sort.Strings(caveats)
	r.Caveats = caveats
	sort.Slice(r.Candidates, func(i, j int) bool { return lessWitness(r.Candidates[i], r.Candidates[j]) })

	r.Digest = canonicalDigest(r)
	return r
}

// staticEffectSets builds, over the bus and DB boundary surface, three keyed
// sets and the disclosed dynamic-bus / opaque-DB counts:
//   - named:     every statically-named bus/db effect key.
//   - reachable: keys with an emitter in the SOUND cone — reachable from an
//     entrypoint without crossing any disclosed gap (CONFIRMED-LIVE candidates).
//   - blind:     keys whose only reaching emitter sits past a disclosed frontier
//     seam (severed-closure, dynamic-bus owner) or blind spot — static abstains
//     there, so an observed effect here is RECLAIMED-LIVE, not an impeachment.
//
// An observed, named effect in NEITHER reachable nor blind is the genuine
// contradiction: unreachable even when every disclosed seam is allowed to be
// crossed (§3). Reachable wins over blind wins over unreachable, so a single
// reachable emitter confirms an effect and a single seam-reachable emitter
// abstains it — both fail toward NOT impeaching (tenet 2).
func staticEffectSets(ix *graph.Index) (named, reachable, blind map[string]bool, busDynamic, dbUnreadable int) {
	named = map[string]bool{}
	reachable = map[string]bool{}
	blind = map[string]bool{}

	var entrySeeds []string
	for _, ep := range ix.Entrypoints() {
		if ep.Fn != "" {
			entrySeeds = append(entrySeeds, ep.Fn)
		}
	}
	if len(entrySeeds) == 0 {
		entrySeeds = ix.Sources()
	}
	reachSet := reachSetOf(ix, entrySeeds)

	// The disclosed seams: every site/owner the graph itself admits it cannot
	// resolve. Seeding reachability from them yields the cone an effect reaches
	// ONLY across a gap static disclosed — the abstain region, not a negative.
	seamSeeds := append([]string(nil), entrySeeds...)
	if fr := ix.Frontier(); fr != nil {
		for _, m := range fr.Markers {
			if ix.Has(m.Site) {
				seamSeeds = append(seamSeeds, m.Site)
			}
			if ix.Has(m.Owner) {
				seamSeeds = append(seamSeeds, m.Owner)
			}
		}
	}
	for _, b := range ix.BlindSpots() {
		if ix.Has(b.Site) {
			seamSeeds = append(seamSeeds, b.Site)
		}
	}
	seamReach := reachSetOf(ix, seamSeeds)

	add := func(key, from string) {
		named[key] = true
		switch {
		case reachSet[from]:
			reachable[key] = true
		case seamReach[from]:
			blind[key] = true
		}
	}
	busEffs, busDynamic := ix.BusEffects()
	for _, be := range busEffs {
		// Only PUBLISH is a caused effect; an inbound CONSUME is an entry, not
		// joined (observedKey excludes it on the behavioral side in lockstep).
		if be.Op != graph.BusPublish {
			continue
		}
		add(graph.BusPublish+" "+be.Event, be.From) // "PUBLISH loan.approved"
	}
	dbEffs, dbUnreadable := ix.DBEffects()
	for _, de := range dbEffs {
		add(DBEffectKey(de.Op, de.Table), de.From)
	}
	return named, reachable, blind, busDynamic, dbUnreadable
}

// reachSetOf is the forward cone of seeds plus the seeds themselves, as a set.
func reachSetOf(ix *graph.Index, seeds []string) map[string]bool {
	m := map[string]bool{}
	for _, fn := range append(ix.Reachable(seeds...), seeds...) {
		m[fn] = true
	}
	return m
}

// observedEffect is one behavioral boundary effect reduced to the join key space.
type observedEffect struct {
	key     string // the canonical join key
	op      string // the raw observed op (enrichment the key drops)
	flow    string
	service string
	entry   string
}

// observedEffects walks the corpus and returns every bus/DB boundary effect as a
// distinct (key, flow, service, entry, op) observation, sorted intrinsically so
// the join output never depends on trace or span arrival order.
func observedEffects(traces []*ir.CanonicalTrace) []observedEffect {
	seen := map[string]bool{}
	var out []observedEffect
	for _, t := range traces {
		if t == nil || t.Root == nil {
			continue
		}
		entry := t.Root.Op
		var walk func(*ir.CanonicalSpan)
		walk = func(s *ir.CanonicalSpan) {
			if s == nil {
				return
			}
			if key, ok := observedKey(s); ok {
				svc := s.Service
				if svc == "" {
					svc = t.Service
				}
				dedup := key + "\x00" + t.Flow + "\x00" + svc + "\x00" + entry + "\x00" + s.Op
				if !seen[dedup] {
					seen[dedup] = true
					out = append(out, observedEffect{key: key, op: s.Op, flow: t.Flow, service: svc, entry: entry})
				}
			}
			for _, g := range s.Children {
				for _, m := range g.Members {
					walk(m)
				}
			}
		}
		walk(t.Root)
	}
	sort.Slice(out, func(i, j int) bool { return lessObserved(out[i], out[j]) })
	return out
}

// observedKey reduces a span to the canonical join key, or ok=false when the span
// is not a Phase-0 CAUSED effect. The cell impeaches an outbound effect a flow
// PRODUCED on a path static says it cannot — so the join is over caused effects:
// a bus PUBLISH and a DB op. An inbound CONSUME or HTTP server span is the flow's
// ENTRY (the left side of attribution), not a caused effect, and is excluded —
// otherwise a consumer's own subscription, whose boundary edge static anchors at
// the registration site (a wiring root, not the handler's cone), reads as a false
// "unreachable" candidate. Outbound HTTP/RPC client calls are caused effects but
// deferred at Phase 0 (label parity, see Audit). A DB span keys through
// ParseDBKey regardless of client/internal kind.
func observedKey(s *ir.CanonicalSpan) (string, bool) {
	if _, op, table, ok := opkey.ParseDBKey(s.Op); ok && op != "" {
		return DBEffectKey(op, table), true
	}
	if s.Kind == ir.KindProducer {
		return s.Op, true // "PUBLISH <event>"
	}
	return "", false
}

func witness(o observedEffect, reach string) Witness {
	return Witness{
		Effect:   o.key,
		Claim:    Claim{Reachability: reach},
		Observed: Observation{Flow: o.flow, Service: o.service, Entry: o.entry, Op: o.op},
		Verdict:  VerdictCandidate,
	}
}

func isBusKey(k string) bool {
	return strings.HasPrefix(k, opkey.PublishPrefix) || strings.HasPrefix(k, opkey.ConsumePrefix)
}

func isDBKey(k string) bool { return strings.HasPrefix(k, "db ") }

// lessWitness is the total intrinsic order over candidates (plan §5: sorted by
// Effect, Flow, Entry, …). Op is the final tie-break so two observations of one
// effect from one flow/entry that differ only in enrichment still order stably.
func lessWitness(a, b Witness) bool {
	if a.Effect != b.Effect {
		return a.Effect < b.Effect
	}
	if a.Observed.Flow != b.Observed.Flow {
		return a.Observed.Flow < b.Observed.Flow
	}
	if a.Observed.Service != b.Observed.Service {
		return a.Observed.Service < b.Observed.Service
	}
	if a.Observed.Entry != b.Observed.Entry {
		return a.Observed.Entry < b.Observed.Entry
	}
	return a.Observed.Op < b.Observed.Op
}

func lessObserved(a, b observedEffect) bool {
	if a.key != b.key {
		return a.key < b.key
	}
	if a.flow != b.flow {
		return a.flow < b.flow
	}
	if a.service != b.service {
		return a.service < b.service
	}
	if a.entry != b.entry {
		return a.entry < b.entry
	}
	return a.op < b.op
}

// corpusDigest pins the audited trace corpus as a SET: the sorted, deduped
// per-trace digests, hashed. So the corpus identity is independent of trace
// arrival order and of a trace appearing twice — the report is a function of
// WHICH canonical traces were seen, not how the slice was assembled (§5).
func corpusDigest(traces []*ir.CanonicalTrace) string {
	seen := map[string]bool{}
	var digs []string
	for _, t := range traces {
		if t == nil {
			continue
		}
		d := canonicalDigest(t)
		if !seen[d] {
			seen[d] = true
			digs = append(digs, d)
		}
	}
	sort.Strings(digs)
	return canonicalDigest(digs)
}

// canonicalDigest is the shared digest primitive (mirrors review.canonicalDigest):
// sha256 over the canonical JSON of a value, so a digest is a pure function of the
// content and recomputable for verification.
func canonicalDigest(v any) string {
	b, err := canonjson.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func plural(n int, noun string) string {
	s := strconv.Itoa(n) + " " + noun
	if n != 1 {
		s += "s"
	}
	return s
}
