// Package policy is the schema and loader for a service's groundwork policy — the
// single, human-authored source of architectural truth that the fitness
// functions enforce. The policy is a CODEOWNERS-gated artifact: if the agent
// under review could author it, it would grade its own homework, so the schema
// is deliberately declarative (no code, no escape hatches beyond explicit
// allow-lists) and is validated strictly on load.
//
// Phase 0 defines and validates the schema; the checks that consume it
// (layering, must-not-reach, I/O budget) arrive with the fitness package.
package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Policy is the whole declared architecture of one service.
type Policy struct {
	Service string `json:"service"`
	Version int    `json:"version"`

	// Substrate records the call-graph algorithm (rta|vta|cha) of the graph this
	// policy was PROPOSED against (`groundwork init`). It is provenance, not a
	// gate input: a reachability proof is sound on any algorithm, but the
	// algorithms differ in PRECISION, so checking a policy against a graph built
	// on a different one can surface spurious findings — a route init proved
	// read-only under a refined substrate (vta) can appear to reach a write under
	// a coarser one (rta over-approximates dynamic dispatch). fitness/verify flag
	// that mismatch so the spurious findings are read as an analyzer artifact, not
	// a regression. Absent on a hand-authored or pre-provenance policy: empty means
	// "unrecorded", never a substrate.
	Substrate string `json:"substrate,omitempty"`

	Layers            []Layer           `json:"layers,omitempty"`
	Layering          *Layering         `json:"layering,omitempty"`
	MustNotReach      []ReachRule       `json:"must_not_reach,omitempty"`
	MustPassThrough   []PassRule        `json:"must_pass_through,omitempty"`
	NoConcurrentReach []ConcurrentRule  `json:"no_concurrent_reach,omitempty"`
	IOBudget          *IOBudget         `json:"io_budget,omitempty"`
	BlindSpotRatchet  *BlindSpotRatchet `json:"blind_spot_ratchet,omitempty"`
	EffectRatchet     *EffectRatchet    `json:"effect_ratchet,omitempty"`
	ImpeachmentGate   *ImpeachmentGate  `json:"impeachment_gate,omitempty"`
	Brokers           map[string]Broker `json:"brokers,omitempty"`
}

// ImpeachmentGate is the opt-in for behavioral impeachment to MOVE the pre-flight
// gate (the behavioral-impeachment plan, §9/§10). Default OFF — observe-first: a
// behaviorally-confirmed must_not_reach breach (VIOLATED) or a require_proof proof
// downgraded to CANT-PROVE is DISCLOSED in the gate output from day one, but blocks
// the merge only once Gate is flipped true, and only ever from a COMMITTED corpus
// (the impeach layer fences live traffic out by construction, §13 crack #2). It is
// the latest, most-gated step in the plan's risk-ascending order, so the opt-in is
// the explicit ratification that the analyzer-unsoundness signal is trusted enough
// to block on.
type ImpeachmentGate struct {
	Gate bool `json:"gate,omitempty"`
}

// Broker is a declared message-broker guarantee, keyed by bus name (e.g. "bus").
// CX-5 chain cards print it verbatim as the *assumed* half of a cross-service
// happens-before chain — the link no static analysis can establish, so it is
// declared, never inferred (D-CX5). The values describe what the transport
// guarantees; the card never strengthens them.
//
// SignedBy is the human warrant. A guarantee a responder reads on a fault card
// needs a name behind it, not the tool's word — so an unsigned broker block is
// printed with its values but flagged UNSIGNED, never silently treated as
// warranted. The tool fills the values; only a person fills SignedBy.
type Broker struct {
	Transport string `json:"transport,omitempty"`
	Delivery  string `json:"delivery,omitempty"`  // at-least-once | at-most-once | exactly-once
	Ordered   string `json:"ordered,omitempty"`   // false | total | per-key:<key>
	Consumers string `json:"consumers,omitempty"` // idempotent | not-idempotent
	Dedup     string `json:"dedup,omitempty"`
	SignedBy  string `json:"signed_by,omitempty"` // human warrant; empty = unsigned (assumed, pending sign-off)
}

// Signed reports whether the broker declaration carries a human warrant.
func (b Broker) Signed() bool { return strings.TrimSpace(b.SignedBy) != "" }

// MergeBrokers folds each loaded service's declared brokers into one fleet-wide
// map. The bus is one thing, so it must have a single declared source: a broker
// named by two policies with DIFFERENT guarantees has no single source (an
// identical re-declaration is harmless). It returns the merged map plus the SORTED
// list of broker names declared with conflicting values — empty when none.
//
// Sorting the conflict names is what makes the caller's refusal byte-identical run
// to run: ranging a policy's Brokers map directly (as the CLI `chains` command did)
// reported whichever conflicting name the map iteration happened to visit, so the
// error text moved between runs. Shared by the CLI and the MCP chains lens so the
// two surfaces conflict on exactly the same condition and word it identically
// (M-4, CLAUDE.md: one source of truth).
func MergeBrokers(perService []map[string]Broker) (map[string]Broker, []string) {
	merged := map[string]Broker{}
	conflict := map[string]bool{}
	for _, brokers := range perService {
		for name, b := range brokers {
			if existing, dup := merged[name]; dup && existing != b {
				conflict[name] = true
			}
			merged[name] = b
		}
	}
	names := make([]string, 0, len(conflict))
	for name := range conflict {
		names = append(names, name)
	}
	sort.Strings(names)
	return merged, names
}

// Layer names an architectural tier and the import-path prefixes that belong to
// it. Order is significant: layers are listed top (entry) to bottom (storage),
// and a call is "skip-level" when it crosses more than one layer downward.
type Layer struct {
	Name     string   `json:"name"`
	Packages []string `json:"packages"`
}

// Layering configures the layering invariant: calls must flow to the adjacent
// layer down (or within a layer), never skipping a layer and never flowing back
// up, except for the allow-listed exceptions. Roots names packages exempt from
// the rule entirely — typically the composition root (main), which legitimately
// constructs every layer.
type Layering struct {
	Allow []Exception `json:"allow,omitempty"`
	Roots []string    `json:"roots,omitempty"`
}

// Exception is one reviewed-and-accepted layering edge. From and To are FQNs (or
// FQN prefixes); an edge matching an exception is never reported, so the gate
// fires only on *new* violations.
type Exception struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

// ReachRule is a negative reachability invariant: no function matching a From
// pattern may transitively reach any target matching a To pattern. To patterns
// match function FQNs or boundary effect targets (e.g. "boundary:bus PUBLISH").
// This is the all-paths safety class — the unique value a test suite cannot
// provide.
//
// RequireProof makes the rule fail closed: when the reachable frontier is blind
// (a reflect/unsafe site or a <dynamic> effect), the default verdict is a
// non-blocking caution ("cannot prove absence"), but a require_proof rule turns
// that unprovability into a Violation. Use it for high-stakes safety invariants
// (auth, payments) where "we could not prove X is unreachable" must not pass CI.
type ReachRule struct {
	Name         string   `json:"name"`
	From         []string `json:"from"`
	To           []string `json:"to"`
	RequireProof bool     `json:"require_proof,omitempty"`
}

// PassRule is a waypoint invariant: every path from a function matching a From
// pattern to a target matching a To pattern must pass through a function
// matching a Through pattern — "every entrypoint-to-DB path goes through the
// auth check". It is the interprocedural sibling of an intraprocedural
// must-precede obligation, needing only the call graph: remove the Through
// nodes and any From→To path that remains is a bypass.
//
// From supports the special selector "entrypoint:*", which matches every graph
// source (node with no first-party caller). This is deliberate: naming the
// handler package instead would let a brand-new handler package silently escape
// the rule — the exact failure mode the rule exists to catch. Exempt the
// composition root and probes via Allow, not by narrowing From.
//
// RequireProof has must_not_reach's discipline: when no bypass is found but the
// walked frontier is blind, the default verdict is a caution ("cannot prove
// every path is guarded"); require_proof turns that into a Violation.
type PassRule struct {
	Name         string      `json:"name"`
	From         []string    `json:"from"`
	To           []string    `json:"to"`
	Through      []string    `json:"through"`
	RequireProof bool        `json:"require_proof,omitempty"`
	Allow        []Exception `json:"allow,omitempty"`
}

// ConcurrentRule forbids reaching a target along a path entered via a
// concurrent edge (a go/defer call site): "no DB writes from goroutines".
// Catches the agent pattern of "make it async" introducing unsupervised
// writes. v1 limitation, disclosed: the graph's concurrent flag conflates
// `go` and `defer` sites; if defer noise appears in practice, the flag is
// split in flowmap first (a small lockstep change planned then, not now).
type ConcurrentRule struct {
	Name         string   `json:"name"`
	To           []string `json:"to"`
	RequireProof bool     `json:"require_proof,omitempty"`
}

// EntrypointSelector is the From selector that expands to every graph source.
const EntrypointSelector = "entrypoint:*"

// Allowed reports whether a (source, target) bypass pair is covered by the
// rule's allow-list. An exception side matches exactly or by an identifier-
// boundary prefix (MatchPrefix — never bare strings.HasPrefix, which would let
// an allow for "boundary:db INSERT users" silently also suppress a real bypass
// writing "boundary:db INSERT users_audit"); an empty side matches anything (an
// entry must declare at least one side — validated on load).
func (r *PassRule) Allowed(source, target string) bool {
	for _, a := range r.Allow {
		if MatchExceptionSide(source, a.From) && MatchExceptionSide(target, a.To) {
			return true
		}
	}
	return false
}

// MatchExceptionSide reports whether one side of an allow/exception entry binds
// the given symbol. An empty pattern is the one-sided wildcard — it matches
// anything, so an exception can name just a From or just a To (validated on load
// to declare at least one side). A non-empty pattern binds at an identifier
// boundary (MatchPrefix), never a bare strings.HasPrefix. This is the SINGLE
// matcher both the layering `exempted` check and PassRule.Allowed key on: a
// one-sided entry (e.g. only To set) must exempt a free-function edge and a
// method edge identically. Splitting it — as `exempted` once did by calling
// MatchPrefix bare — makes MatchPrefix(from, "") false for a free function but
// accidentally true for a method-shaped From, so the same exception half-works,
// split by the receiver shape of the edge it should exempt (H-6).
func MatchExceptionSide(s, pat string) bool {
	return pat == "" || MatchPrefix(s, pat)
}

// IOBudget caps the external write effects reachable from a single entrypoint —
// the side-effect blowout guard. Writes are boundary edges with an
// outbound-sync/outbound-async kind; reads do not count.
type IOBudget struct {
	MaxWritesPerRoute int `json:"max_writes_per_route"`
}

// BlindSpotRatchet is the drift ratchet on the graph's own soundness: no new
// blind spots base→branch without a reviewed allow-list entry. Every other
// check is only as good as the substrate; unchecked growth in dynamic dispatch
// erodes them all silently. Review always reports new (unallowed) blind spots;
// Gate makes them merge-blocking only when Gate is true, so adopters observe
// first and gate once the baseline is clean.
type BlindSpotRatchet struct {
	Gate  bool                 `json:"gate,omitempty"`
	Allow []BlindSpotException `json:"allow,omitempty"`
}

// BlindSpotException is one reviewed-and-accepted blind spot. Site matches the
// blind spot's site exactly or as an identifier-boundary prefix (policy.MatchPrefix,
// the same convention as a layering Exception) — a bare prefix would let an entry
// for "reflectutil" silently also allow a new, distinct "reflectutil2" blind spot.
// An empty Kind matches any kind.
type BlindSpotException struct {
	Kind   string `json:"kind,omitempty"`
	Site   string `json:"site"`
	Reason string `json:"reason,omitempty"`
}

// Allows reports whether a blind spot with this kind and site is covered by an
// allow-list entry. A nil ratchet allows nothing (every new blind spot is
// reported — just never gated).
func (r *BlindSpotRatchet) Allows(kind, site string) bool {
	if r == nil {
		return false
	}
	for _, a := range r.Allow {
		if a.Kind != "" && a.Kind != kind {
			continue
		}
		if MatchPrefix(site, a.Site) {
			return true
		}
	}
	return false
}

// GatesBlindSpots reports whether new blind spots block the pre-flight gate.
func (p *Policy) GatesBlindSpots() bool {
	return p.BlindSpotRatchet != nil && p.BlindSpotRatchet.Gate
}

// EffectRatchet is the drift ratchet on the external write surface: no new
// boundary write target base→branch without a reviewed allow-list entry. The
// sibling of BlindSpotRatchet with the same lifecycle — review always reports
// new (unallowed) write targets; Gate makes them merge-blocking only when Gate
// is true, so adopters observe first and gate once the baseline is clean.
//
// The ratchet sees new effect LABELS, not new uses of an existing label —
// that residual is the route-delta section's job. Its soundness against
// dynamic laundering (routing a new write through dynamic dispatch so the
// label collapses to an existing <dynamic>) leans on the blind-spot ratchet:
// the new dispatch site is what fires. Gate this ratchet without gating
// blind_spot_ratchet and that escape stays open.
type EffectRatchet struct {
	Gate  bool              `json:"gate,omitempty"`
	Allow []EffectException `json:"allow,omitempty"`
}

// EffectException is one reviewed-and-accepted write target. Target matches the
// effect label (sans "boundary:", e.g. "db INSERT audit_log") exactly or as an
// identifier-boundary prefix (policy.MatchPrefix): an op-level target like
// "db INSERT" still binds every INSERT (the space is a boundary), but a
// table-level target "db INSERT users" no longer silently also allows a new,
// distinct "db INSERT users_audit" write — list such tables explicitly. Empty
// targets are rejected at load (a "" would allow every write).
type EffectException struct {
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
}

// Allows reports whether a write-effect label is covered by an allow-list
// entry. A nil ratchet allows nothing (every new write target is reported —
// just never gated).
func (r *EffectRatchet) Allows(label string) bool {
	if r == nil {
		return false
	}
	for _, a := range r.Allow {
		if MatchPrefix(label, a.Target) {
			return true
		}
	}
	return false
}

// GatesEffects reports whether new write targets block the pre-flight gate.
func (p *Policy) GatesEffects() bool {
	return p.EffectRatchet != nil && p.EffectRatchet.Gate
}

// GatesImpeachment reports whether behaviorally-confirmed impeachments block the
// pre-flight gate. Default false (nil opt-in) — disclosed but never blocking until
// explicitly ratified (§9/§10 observe-first).
func (p *Policy) GatesImpeachment() bool {
	return p.ImpeachmentGate != nil && p.ImpeachmentGate.Gate
}

// EffectRatchetCouplingCaution returns a disclosure when the policy gates the
// effect ratchet but NOT the blind-spot ratchet — the one configuration where the
// effect ratchet's soundness backstop is off. A new write laundered through dynamic
// dispatch collapses to an existing "<dynamic>" label and escapes the effect
// ratchet's label diff; blind_spot_ratchet (which fires on the new dispatch site)
// is its only backstop, and the "gate the effect ratchet first" rollout advice
// lands exactly here. "" when there is nothing to flag.
//
// This is the SINGLE SOURCE of both the predicate and the wording: policy-check and
// fitness both call it, so the two surfaces cannot disagree (one source of truth).
// The EffectRatchet type doc states the soundness dependency in prose; this makes it
// self-checking. It is advisory by construction — a Caution, never a gate flip.
func (p *Policy) EffectRatchetCouplingCaution() string {
	if p.GatesEffects() && !p.GatesBlindSpots() {
		return "effect_ratchet gates but blind_spot_ratchet does not — a new write laundered through dynamic dispatch collapses to an existing <dynamic> label and escapes the effect ratchet; gate blind_spot_ratchet too to close the escape"
	}
	return ""
}

// Load decodes and validates a policy from JSON. Unknown fields are rejected so a
// typo'd or stale key is a load error, not a silently-ignored rule.
func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("groundwork/policy: %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("groundwork/policy: %s: %w", path, err)
	}
	return &p, nil
}

// Validate checks the policy is internally well-formed: it must name a service,
// declare layers with unique names and non-empty package lists, and only
// reference declared layers. It does not check the policy against any graph —
// that is the fitness functions' job.
func (p *Policy) Validate() error {
	if strings.TrimSpace(p.Service) == "" {
		return fmt.Errorf("service is required")
	}
	if p.Version == 0 {
		return fmt.Errorf("version is required")
	}
	seen := make(map[string]bool, len(p.Layers))
	for i, l := range p.Layers {
		if strings.TrimSpace(l.Name) == "" {
			return fmt.Errorf("layers[%d]: name is required", i)
		}
		if seen[l.Name] {
			return fmt.Errorf("layers[%d]: duplicate layer name %q", i, l.Name)
		}
		seen[l.Name] = true
		if len(l.Packages) == 0 {
			return fmt.Errorf("layer %q: at least one package prefix is required", l.Name)
		}
		if err := noEmptyPattern("layer", l.Name, "packages", l.Packages); err != nil {
			return err
		}
	}
	if p.Layering != nil {
		if len(p.Layers) == 0 {
			return fmt.Errorf("layering is configured but no layers are declared")
		}
		// An exception with both sides empty prefix-matches every edge — one
		// such entry silently disables the whole layering invariant, the exact
		// inert-guardrail failure mode this validator exists to catch.
		for i, a := range p.Layering.Allow {
			if a.From == "" && a.To == "" {
				return fmt.Errorf("layering: allow[%d] must declare from and/or to — an empty entry exempts every edge", i)
			}
		}
	}
	// Rule names are identity: findings carry them and the exceptions audit
	// attributes suppressed findings to entries by name, so a duplicate name would
	// merge two distinct rules' provenance. must_pass_through already guards this;
	// the reach rules must too (each rule kind has its own namespace).
	reachNames := make(map[string]bool, len(p.MustNotReach))
	for i, r := range p.MustNotReach {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("must_not_reach[%d]: name is required", i)
		}
		if reachNames[r.Name] {
			return fmt.Errorf("must_not_reach[%d]: duplicate name %q", i, r.Name)
		}
		reachNames[r.Name] = true
		if len(r.From) == 0 || len(r.To) == 0 {
			return fmt.Errorf("must_not_reach[%d] (%s): from and to are both required", i, r.Name)
		}
		if err := noSelector("must_not_reach", r.Name, "to", r.To); err != nil {
			return err
		}
		if err := noEmptyPattern("must_not_reach", r.Name, "from", r.From); err != nil {
			return err
		}
		if err := noEmptyPattern("must_not_reach", r.Name, "to", r.To); err != nil {
			return err
		}
	}
	concNames := make(map[string]bool, len(p.NoConcurrentReach))
	for i, r := range p.NoConcurrentReach {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("no_concurrent_reach[%d]: name is required", i)
		}
		if concNames[r.Name] {
			return fmt.Errorf("no_concurrent_reach[%d]: duplicate name %q", i, r.Name)
		}
		concNames[r.Name] = true
		if len(r.To) == 0 {
			return fmt.Errorf("no_concurrent_reach[%d] (%s): to is required", i, r.Name)
		}
		if err := noSelector("no_concurrent_reach", r.Name, "to", r.To); err != nil {
			return err
		}
		if err := noEmptyPattern("no_concurrent_reach", r.Name, "to", r.To); err != nil {
			return err
		}
	}
	passNames := make(map[string]bool, len(p.MustPassThrough))
	for i, r := range p.MustPassThrough {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("must_pass_through[%d]: name is required", i)
		}
		// Names are identity: findings carry them, and the exceptions audit
		// attributes suppressed findings to entries by rule name.
		if passNames[r.Name] {
			return fmt.Errorf("must_pass_through[%d]: duplicate name %q", i, r.Name)
		}
		passNames[r.Name] = true
		if len(r.From) == 0 || len(r.To) == 0 || len(r.Through) == 0 {
			return fmt.Errorf("must_pass_through[%d] (%s): from, to and through are all required", i, r.Name)
		}
		for j, a := range r.Allow {
			if a.From == "" && a.To == "" {
				return fmt.Errorf("must_pass_through[%d] (%s): allow[%d] must declare from and/or to", i, r.Name, j)
			}
		}
		if err := noSelector("must_pass_through", r.Name, "to", r.To); err != nil {
			return err
		}
		if err := noSelector("must_pass_through", r.Name, "through", r.Through); err != nil {
			return err
		}
		for _, f := range [3]struct {
			field string
			pats  []string
		}{{"from", r.From}, {"to", r.To}, {"through", r.Through}} {
			if err := noEmptyPattern("must_pass_through", r.Name, f.field, f.pats); err != nil {
				return err
			}
		}
	}
	if p.IOBudget != nil && p.IOBudget.MaxWritesPerRoute < 0 {
		return fmt.Errorf("io_budget.max_writes_per_route must be non-negative")
	}
	if p.BlindSpotRatchet != nil {
		for i, a := range p.BlindSpotRatchet.Allow {
			if strings.TrimSpace(a.Site) == "" {
				return fmt.Errorf("blind_spot_ratchet.allow[%d]: site is required", i)
			}
		}
	}
	if p.EffectRatchet != nil {
		for i, a := range p.EffectRatchet.Allow {
			if strings.TrimSpace(a.Target) == "" {
				return fmt.Errorf("effect_ratchet.allow[%d]: target is required — an empty entry allows every write", i)
			}
		}
	}
	for name, b := range p.Brokers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("brokers: a broker name is required (the bus key the card prints)")
		}
		// A printed guarantee must not assert a value outside its own
		// vocabulary — a typo'd "delivery": "atleastonce" would read on a fault
		// card as a real (false) guarantee. Validate the closed-vocabulary
		// fields; free-form fields (transport, dedup) are printed as-is.
		if b.Delivery != "" && !oneOf(b.Delivery, "at-least-once", "at-most-once", "exactly-once") {
			return fmt.Errorf("brokers[%q].delivery %q: want at-least-once | at-most-once | exactly-once", name, b.Delivery)
		}
		if b.Ordered != "" && b.Ordered != "false" && b.Ordered != "total" && !strings.HasPrefix(b.Ordered, "per-key:") {
			return fmt.Errorf("brokers[%q].ordered %q: want false | total | per-key:<key>", name, b.Ordered)
		}
		if b.Consumers != "" && !oneOf(b.Consumers, "idempotent", "not-idempotent") {
			return fmt.Errorf("brokers[%q].consumers %q: want idempotent | not-idempotent", name, b.Consumers)
		}
	}
	return nil
}

// oneOf reports whether s equals any of the allowed values.
func oneOf(s string, allowed ...string) bool {
	for _, a := range allowed {
		if s == a {
			return true
		}
	}
	return false
}

// RootPackages returns the composition-root package paths (from the layering
// config, if any) — the packages whose entrypoints are not service routes. It is
// shared by the layering check (which exempts calls out of a root) and the I/O
// budget (for which main is not a route).
func (p *Policy) RootPackages() []string {
	if p.Layering == nil {
		return nil
	}
	return p.Layering.Roots
}

// LayerOf returns the name of the layer owning pkgPath (the longest matching
// package prefix wins, so a nested package can override its parent), or "" if no
// declared layer claims it.
func (p *Policy) LayerOf(pkgPath string) string {
	best, bestLen := "", -1
	for _, l := range p.Layers {
		for _, pre := range l.Packages {
			if MatchPrefix(pkgPath, pre) && len(pre) > bestLen {
				best, bestLen = l.Name, len(pre)
			}
		}
	}
	return best
}

// LayerRank returns the 0-based position of a layer in the declared top-to-bottom
// order, and whether it was found.
func (p *Policy) LayerRank(name string) (int, bool) {
	for i, l := range p.Layers {
		if l.Name == name {
			return i, true
		}
	}
	return 0, false
}

// LayerNames returns the declared layer names in order.
func (p *Policy) LayerNames() []string {
	out := make([]string, len(p.Layers))
	for i, l := range p.Layers {
		out[i] = l.Name
	}
	return out
}

// MatchPrefix reports whether s equals prefix or is bound by it AT AN IDENTIFIER
// BOUNDARY: the byte of s immediately after the prefix must be a non-identifier
// byte (e.g. '.', '/', ' ', '['). This is the one matcher every gate, ratchet,
// and layer assignment uses to decide "does this pattern name this symbol?", so a
// prefix can name a function, a receiver type, a whole package path, or a boundary
// label and bind all its members ("...internal/app" → "...internal/app/sub",
// "db INSERT" → "db INSERT users") WITHOUT a bare strings.HasPrefix splitting an
// identifier and binding an UNRELATED symbol — the prefix-collision that let scope
// "app" admit a sibling package "application", an exception "GetUser" suppress
// "GetUserAvatar", and a ratchet "users" allow a new "users_audit" write target.
// fitness.matchAny delegates here so the rule-pattern matcher and the gate matcher
// can never diverge. An exact match always binds.
func MatchPrefix(s, prefix string) bool {
	if s == prefix {
		return true
	}
	return len(s) > len(prefix) && strings.HasPrefix(s, prefix) && !isIdentByte(s[len(prefix)])
}

// isIdentByte reports whether b can appear inside a Go identifier, so a prefix
// ending immediately before it would split that identifier rather than bind it at
// a boundary. FQNs, package paths, and boundary labels here are ASCII.
func isIdentByte(b byte) bool {
	return b == '_' ||
		('a' <= b && b <= 'z') ||
		('A' <= b && b <= 'Z') ||
		('0' <= b && b <= '9')
}

// noSelector rejects the entrypoint:* selector in a position where it has no
// defined meaning. Selectors are only valid where a rule kind explicitly
// expands them (From positions); accepting one anywhere else would make the
// pattern match nothing — a rule that silently binds nothing, the inert-
// guardrail failure mode. Fail at load, not at match time.
func noSelector(kind, rule, field string, patterns []string) error {
	for _, p := range patterns {
		if p == EntrypointSelector {
			return fmt.Errorf("%s (%s): %q is not valid in %s — selectors are only defined for from", kind, rule, EntrypointSelector, field)
		}
	}
	return nil
}

// noEmptyPattern rejects empty (or whitespace-only) elements in a pattern list.
// Every matcher treats patterns as exact-or-prefix, so "" matches every symbol:
// in a From/To/Through it binds or trips the rule everywhere, and in a layer's
// packages it claims every package for that layer. Either way the policy stops
// meaning what it says — fail at load, not at match time.
func noEmptyPattern(kind, rule, field string, patterns []string) error {
	for i, p := range patterns {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%s (%s): %s[%d] is empty, which would prefix-match everything", kind, rule, field, i)
		}
	}
	return nil
}
