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
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Policy is the whole declared architecture of one service.
type Policy struct {
	Service      string      `json:"service"`
	Version      int         `json:"version"`
	Layers       []Layer     `json:"layers,omitempty"`
	Layering     *Layering   `json:"layering,omitempty"`
	MustNotReach []ReachRule `json:"must_not_reach,omitempty"`
	IOBudget     *IOBudget   `json:"io_budget,omitempty"`
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

// IOBudget caps the external write effects reachable from a single entrypoint —
// the side-effect blowout guard. Writes are boundary edges with an
// outbound-sync/outbound-async kind; reads do not count.
type IOBudget struct {
	MaxWritesPerRoute int `json:"max_writes_per_route"`
}

// Load decodes and validates a policy from JSON. Unknown fields are rejected so a
// typo'd or stale key is a load error, not a silently-ignored rule.
func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
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
	if p.IOBudget != nil && p.IOBudget.MaxWritesPerRoute < 0 {
		return fmt.Errorf("io_budget.max_writes_per_route must be non-negative")
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
