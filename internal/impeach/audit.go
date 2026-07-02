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

	// TraceIdentity is the NUMERATOR's code identity (the deployed-commit stamp
	// the ladder's code-identity rung matches against GraphStamp). Absent ("")
	// means unestablished — by representation that forces every candidate to
	// VERSION-SKEW (§5). CorpusDigest pins the exact canonical trace set audited.
	TraceIdentity string `json:"trace_identity,omitempty"`
	CorpusDigest  string `json:"corpus_digest"`

	// CaptureProvenance is the self-declared capture fidelity (production |
	// integration | synthetic). A synthetic/absent capture caps every candidate at
	// CAPTURE-UNTRUSTED (§4 rung 5). Recorded verbatim, never inferred (§5).
	CaptureProvenance string `json:"capture_provenance,omitempty"`

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
	Effect    string      `json:"effect"`              // the canonical join key (the §7 key space)
	Claim     Claim       `json:"claim"`               // the static negative under test
	Observed  Observation `json:"observed"`            // the runtime counterexample
	Severance *Severance  `json:"severance,omitempty"` // the localization of WHERE static lost it (§6, Phase 2/3)
	Rungs     []Rung      `json:"rungs"`               // the FULL ordered downgrade ladder (§4), recorded whole
	Verdict   string      `json:"verdict"`             // CANDIDATE | <downgrade> | IMPEACHMENT | VIOLATED (§5/§9)

	// Repair is the Phase-4 proposed substrate change (§5/§8): present on
	// IMPEACHMENT/VIOLATED, nil otherwise. It is a PROPOSAL — never enacted here;
	// the loop proposes, a human ratifies (§8). omitempty so a Phase-0..3 report
	// (which never proposes) serializes and digests identically.
	Repair *ProposedRepair `json:"repair,omitempty"`

	// chain is the causal span chain the L1 localizer walks (§6/§7). Unexported, so
	// it never serializes or perturbs the digest — the spans' canonical ops already
	// ride into Observation.CausalPath; this carries the Attrs (the FQN tags) the
	// walk needs and that the serialized form deliberately omits.
	chain []*ir.CanonicalSpan
}

// Claim is the static side of the contradiction.
type Claim struct {
	Reachability string `json:"reachability"` // ReachUnreachable | ReachAbsent

	// Rules are the must_not_reach rule names whose `to` binds this effect — the
	// SATISFIED (proven-absent, §14-C) proofs this impeachment touches. A bare
	// impeachment downgrades each to CANT-PROVE; a witnessed breach of one whose
	// `from` also binds the entry is a VIOLATED (§9). Populated only by the Phase-5
	// verdict integration (Resolve); empty in a Phase-0..3 report (omitempty so the
	// digest is unchanged), because the ladder has no policy to read.
	Rules []string `json:"rules,omitempty"`
}

// Observation is the behavioral side. Entry is the coarse L0 anchor (the
// observed trace root's op); Op is the raw observed op key, which carries the
// behavioral enrichment the system-agnostic Effect key drops (e.g. the DB
// system in "DB postgres DELETE ledger").
type Observation struct {
	Flow    string `json:"flow"`
	Service string `json:"service,omitempty"`
	Entry   string `json:"entry,omitempty"`
	// EntryDiscovered records whether the graph modeled Entry as a root (§5): the
	// missed-root vs missed-edge distinction the severance walk turns on (§6). Set
	// during localization, so it is the same entrypoint join Severance is derived
	// from, not a second guess.
	EntryDiscovered bool `json:"entry_discovered"`
	// CausalPath is the canonical op chain entry→effect (no ids, no timestamps,
	// §5) — the run-independent evidence the L1 severance walk projects onto the
	// graph. Disclosure of WHAT was observed, distinct from the localized Site.
	CausalPath []string `json:"causal_path,omitempty"`
	Op         string   `json:"op"`
}

// Audit joins a stamped graph against a canonical trace corpus and returns the
// witness report, each candidate classified through the downgrade ladder (§4):
// IMPEACHMENT or one specific downgrade. It is a pure function of (ix, traces,
// prov): all ordering is intrinsic (effect key, flow, op), no map iteration or
// arrival order reaches the output, so the report — and its digest — is
// byte-identical across runs. prov supplies the ladder's capture-side inputs
// (code identity, capture fidelity), so the verdicts and digest depend on it; a
// zero Provenance fails the capture-side rungs closed (every candidate caps at a
// downgrade, never IMPEACHMENT).
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
func Audit(service string, ix *graph.Index, traces []*ir.CanonicalTrace, prov Provenance) Report {
	// Resolve the corpus's code identity: a LIVE corpus self-describes through the
	// traces' own Stamp; a committed (stampless) corpus takes the caller's injected
	// identity. The resolution feeds the ladder's code-identity rung.
	prov.TraceIdentity = resolveIdentity(traces, prov)
	// Resolve the capture fidelity grade the SAME way (§12.6): a graded corpus
	// self-describes (the producer set it), an ungraded one takes the caller's
	// grade, and a caller grade that CONTRADICTS the corpus fails closed — so the
	// capture-fidelity rung can no longer be asserted divorced from the capture.
	prov.Capture = resolveCaptureProvenance(traces, prov)
	r := Report{
		Service:           service,
		GraphStamp:        ix.Stamp(),
		GraphTool:         ix.Tool(),
		GraphAlgo:         ix.Algo(),
		TraceIdentity:     prov.TraceIdentity,
		CorpusDigest:      corpusDigest(traces),
		CaptureProvenance: prov.Capture,
		Candidates:        []Witness{},
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
	caveats = append(caveats, "Phase 1 downgrade ladder (§4): code-identity and capture-fidelity consume caller-supplied provenance absent from the trace model today (§14-D); without it every candidate caps at a downgrade (VERSION-SKEW/CAPTURE-UNTRUSTED), never IMPEACHMENT")

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

	// Localize each candidate (Phase 2, §6): project its coarse (entry → effect)
	// anchors onto the graph and record WHERE static lost the effect (the Site) plus
	// the known/unknown frontier sort. The proof obligation is folded in — a
	// candidate whose effect turns out to be statically reproducible (Kind
	// SeveranceNone) is a self-inconsistency, disclosed in a caveat, never a
	// fabricated seam. Done before classify so EntryDiscovered is set when the
	// ladder reads the witness, and before the sort/digest so the localization is
	// part of the byte-identical artifact.
	lz := newLocalizer(ix)
	selfInconsistent := 0
	for i := range r.Candidates {
		sev, discovered, ok := lz.localize(r.Candidates[i])
		s := sev
		r.Candidates[i].Severance = &s
		r.Candidates[i].Observed.EntryDiscovered = discovered
		if !ok {
			selfInconsistent++
		}
	}
	if selfInconsistent > 0 {
		caveats = append(caveats, plural(selfInconsistent, "candidate")+" is self-inconsistent (the effect is statically reproducible along the observed anchors): localized to no severance, never impeached (§6 proof obligation)")
	}

	// Classify each candidate through the downgrade ladder (§4): CANDIDATE becomes
	// IMPEACHMENT or a specific downgrade, with the full ordered ladder recorded.
	// Done before the sort/digest so the ladder is part of the byte-identical
	// artifact, and after the candidate set is final so the corpus-level provenance
	// (graph stamp, supplied identity/capture) is the same for every witness.
	for i := range r.Candidates {
		r.Candidates[i].Rungs, r.Candidates[i].Verdict = classify(r.Candidates[i], ix, service, prov)
	}

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

	entrySeeds := entrySeedsOf(ix)
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
		add(BusEffectKey(be.Event), be.From) // "PUBLISH loan.approved" (single-sourced; parity with observedKey)
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

// entrySeedsOf is the single source of truth for "the discovered roots": the
// named entrypoints' handler functions, falling back to the graph's structural
// sources when the graph names no entrypoint. Shared by staticEffectSets (which
// computes the reachable cone) and rootReachOf (which the localizer walks against),
// so the two never drift on what counts as a root.
func entrySeedsOf(ix *graph.Index) []string {
	var seeds []string
	for _, ep := range ix.Entrypoints() {
		if ep.Fn != "" {
			seeds = append(seeds, ep.Fn)
		}
	}
	if len(seeds) == 0 {
		seeds = ix.Sources()
	}
	return seeds
}

// rootReachOf is the set of functions reachable from any discovered root (the
// roots included) — the SOUND cone the L1 severance walk tests a path node
// against: a node OUTSIDE it is severed from every root, the seam's downstream
// side (§6).
func rootReachOf(ix *graph.Index) map[string]bool {
	return reachSetOf(ix, entrySeedsOf(ix))
}

// observedEffect is one behavioral boundary effect reduced to the join key space.
// path is the causal span chain root→effect (inclusive), the L1 severance walk's
// input (§6/§7); it is intrinsic evidence, not serialized directly — the witness
// projects it to Observation.CausalPath (ops) and the localizer maps its internal
// spans to graph nodes.
type observedEffect struct {
	key     string // the canonical join key
	op      string // the raw observed op (enrichment the key drops)
	flow    string
	service string
	entry   string
	path    []*ir.CanonicalSpan
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
		// stack is the ancestor chain root→current, so an effect span records the
		// full causal path it sits on (the L1 walk's input, §6).
		var stack []*ir.CanonicalSpan
		var walk func(*ir.CanonicalSpan)
		walk = func(s *ir.CanonicalSpan) {
			if s == nil {
				return
			}
			stack = append(stack, s)
			if key, ok := observedKey(s); ok {
				svc := s.Service
				if svc == "" {
					svc = t.Service
				}
				path := append([]*ir.CanonicalSpan(nil), stack...)
				// The causal path is part of identity: two paths to the same effect
				// are distinct witnesses, ordered deterministically by their op chain,
				// so a path never reaches the output by trace arrival order (§5).
				dedup := key + "\x00" + t.Flow + "\x00" + svc + "\x00" + entry + "\x00" + s.Op + "\x00" + pathSig(path)
				if !seen[dedup] {
					seen[dedup] = true
					out = append(out, observedEffect{key: key, op: s.Op, flow: t.Flow, service: svc, entry: entry, path: path})
				}
			}
			for _, g := range s.Children {
				for _, m := range g.Members {
					walk(m)
				}
			}
			stack = stack[:len(stack)-1]
		}
		walk(t.Root)
	}
	sort.Slice(out, func(i, j int) bool { return lessObserved(out[i], out[j]) })
	return out
}

// pathSig is the intrinsic signature of a causal span chain — each span's op AND
// its `flowmap.fqn` tag, joined in order — used to key and order observations so
// the chain never reaches the output by arrival order (determinism, §5/§10). The
// FQN tag is folded in because it is part of the path's IDENTITY at L1: two paths
// with the same op chain but different tags traverse different functions and
// localize to different Sites, so collapsing them on op alone would drop one path's
// distinct severance (and make WHICH survives depend on trace arrival order).
func pathSig(path []*ir.CanonicalSpan) string {
	parts := make([]string, len(path))
	for i, s := range path {
		parts[i] = s.Op
		if tag := s.Attrs[FQNTagKey]; tag != "" {
			parts[i] += "\x1e" + tag
		}
	}
	return strings.Join(parts, "\x1f")
}

// causalOps projects a causal span chain to its ordered op list — the disclosed
// CausalPath (§5): canonical span sigs, entry→effect, no ids, no timestamps.
func causalOps(path []*ir.CanonicalSpan) []string {
	ops := make([]string, 0, len(path))
	for _, s := range path {
		ops = append(ops, s.Op)
	}
	return ops
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
		// Single-source the key through BusEffectKey (parity with the static side,
		// TestBusEffectKeyParity): strip canon's PUBLISH prefix and re-key. A producer
		// op always carries the prefix (canon invariant); if one ever does not, key it
		// raw so it fails to match rather than minting a double-prefixed phantom.
		if event, found := strings.CutPrefix(s.Op, opkey.PublishPrefix); found {
			return BusEffectKey(event), true
		}
		return s.Op, true
	}
	return "", false
}

func witness(o observedEffect, reach string) Witness {
	return Witness{
		Effect:   o.key,
		Claim:    Claim{Reachability: reach},
		Observed: Observation{Flow: o.flow, Service: o.service, Entry: o.entry, Op: o.op, CausalPath: causalOps(o.path)},
		Verdict:  VerdictCandidate,
		chain:    o.path,
	}
}

func isBusKey(k string) bool {
	return strings.HasPrefix(k, opkey.PublishPrefix) || strings.HasPrefix(k, opkey.ConsumePrefix)
}

func isDBKey(k string) bool { return strings.HasPrefix(k, "db ") }

// lessWitness is the total intrinsic order over candidates (plan §5: sorted by
// Effect, Flow, Entry, …). The causal path is the FINAL tie-break: since Phase 3
// makes two paths to one effect DISTINCT witnesses (the dedup keys on pathSig),
// two candidates can share (Effect, Flow, Service, Entry, Op) yet differ only in
// path — so the order must break on pathSig here too, exactly as lessObserved
// does, or the non-stable candidate sort would order them on arrival. The chain
// carries the FQN tags pathSig folds in (Observation.CausalPath drops them), so the
// tie-break is over the full path identity.
func lessWitness(a, b Witness) bool {
	ka, kb := witnessSortKey(a), witnessSortKey(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return ka[i] < kb[i]
		}
	}
	return false
}

// witnessSortKey is the ONE definition of a witness's intrinsic sort identity (§5):
// effect, then the observation's flow/service/entry/op, then the causal-path
// signature (which folds the FQN tags, so two paths to one effect order distinctly).
// lessWitness compares it element-wise and the determinism fuzz mirrors it through
// THIS helper, so the comparator and its property test cannot drift on which fields
// constitute identity (CLAUDE.md one-source). Equal keys ⇒ equal witness for the sort;
// the dedup (audit.go observedEffects) guarantees no two candidates share one.
func witnessSortKey(w Witness) [6]string {
	return [6]string{
		w.Effect,
		w.Observed.Flow,
		w.Observed.Service,
		w.Observed.Entry,
		w.Observed.Op,
		pathSig(w.chain),
	}
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
	if a.op != b.op {
		return a.op < b.op
	}
	// Two observations of one effect from one flow/entry/op that differ only in
	// causal path order stably by the path signature (determinism, §5).
	return pathSig(a.path) < pathSig(b.path)
}

// corpusDigest pins the audited trace corpus as a SET: the sorted, deduped
// per-trace digests, hashed. So the corpus identity is independent of trace
// arrival order and of a trace appearing twice — the report is a function of
// WHICH canonical traces were seen, not how the slice was assembled (§5).
//
// Each trace is digested with its code-identity Stamp zeroed, as in
// golden.canonicalBytes: the Stamp is run-varying provenance EXCLUDED from trace
// equality, so two captures of one flow on different deploys are the same
// canonical trace and yield the same corpus digest. The corpus's deploy identity
// is carried separately as Report.TraceIdentity, never folded into the structural
// digest (else this digest would churn per deploy).
//
// The capture-Provenance grade is DELIBERATELY RETAINED here — the one field where
// this digest diverges from golden.canonicalBytes, which zeroes it. For the golden
// the grade is not behavioral, so equality must ignore it (a "production" re-drive
// and an "integration" re-drive of identical behavior are the same snapshot). For
// the AUDIT the grade IS identity: a production-grade corpus and an
// integration-grade corpus of the same flow license different verdicts (only the
// trusted grades promote an impeachment), so they must digest DIFFERENTLY — folding
// the grade in is what keeps the audit's identity honest about what it was allowed
// to conclude.
func corpusDigest(traces []*ir.CanonicalTrace) string {
	seen := map[string]bool{}
	var digs []string
	for _, t := range traces {
		if t == nil {
			continue
		}
		cp := *t
		cp.Stamp = "" // Provenance intentionally kept (see doc): grade is audit identity
		d := canonicalDigest(&cp)
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
//
// It fails LOUD, exactly like its review sibling: canonjson only fails on a value
// it cannot represent (a channel/func/complex field), which none of the impeach
// types digested here carry — a failure means the IR is corrupt. Swallowing it to
// "" would collapse EVERY failing trace onto one shared "" corpus entry, deduping
// distinct impeachment evidence into one cell (M-30) — a silent fail-open in an
// otherwise rigorously fail-closed package. Refuse rather than corrupt the corpus.
func canonicalDigest(v any) string {
	b, err := canonjson.Marshal(v)
	if err != nil {
		panic("impeach: marshal for digest failed on a value assumed always representable (corrupt IR): " + err.Error())
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
