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
	"strings"
)

// Policy is the whole declared architecture of one service.
type Policy struct {
	Service           string            `json:"service"`
	Version           int               `json:"version"`
	Layers            []Layer           `json:"layers,omitempty"`
	Layering          *Layering         `json:"layering,omitempty"`
	MustNotReach      []ReachRule       `json:"must_not_reach,omitempty"`
	MustPassThrough   []PassRule        `json:"must_pass_through,omitempty"`
	NoConcurrentReach []ConcurrentRule  `json:"no_concurrent_reach,omitempty"`
	IOBudget          *IOBudget         `json:"io_budget,omitempty"`
	BlindSpotRatchet  *BlindSpotRatchet `json:"blind_spot_ratchet,omitempty"`
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
// rule's allow-list. An exception side matches exactly or by prefix; an empty
// side matches anything (an entry must declare at least one side — validated on
// load).
func (r *PassRule) Allowed(source, target string) bool {
	match := func(s, pat string) bool {
		return pat == "" || s == pat || strings.HasPrefix(s, pat)
	}
	for _, a := range r.Allow {
		if match(source, a.From) && match(target, a.To) {
			return true
		}
	}
	return false
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
// blind spot's site exactly or by prefix (the same convention as a layering
// Exception); an empty Kind matches any kind.
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
		if site == a.Site || hasPrefix(site, a.Site) {
			return true
		}
	}
	return false
}

// GatesBlindSpots reports whether new blind spots block the pre-flight gate.
func (p *Policy) GatesBlindSpots() bool {
	return p.BlindSpotRatchet != nil && p.BlindSpotRatchet.Gate
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
	}
	if p.Layering != nil && len(p.Layers) == 0 {
		return fmt.Errorf("layering is configured but no layers are declared")
	}
	for i, r := range p.MustNotReach {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("must_not_reach[%d]: name is required", i)
		}
		if len(r.From) == 0 || len(r.To) == 0 {
			return fmt.Errorf("must_not_reach[%d] (%s): from and to are both required", i, r.Name)
		}
	}
	for i, r := range p.NoConcurrentReach {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("no_concurrent_reach[%d]: name is required", i)
		}
		if len(r.To) == 0 {
			return fmt.Errorf("no_concurrent_reach[%d] (%s): to is required", i, r.Name)
		}
	}
	for i, r := range p.MustPassThrough {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("must_pass_through[%d]: name is required", i)
		}
		if len(r.From) == 0 || len(r.To) == 0 || len(r.Through) == 0 {
			return fmt.Errorf("must_pass_through[%d] (%s): from, to and through are all required", i, r.Name)
		}
		for j, a := range r.Allow {
			if a.From == "" && a.To == "" {
				return fmt.Errorf("must_pass_through[%d] (%s): allow[%d] must declare from and/or to", i, r.Name, j)
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
	return nil
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
			if hasPrefix(pkgPath, pre) && len(pre) > bestLen {
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

func hasPrefix(s, prefix string) bool { return strings.HasPrefix(s, prefix) }
