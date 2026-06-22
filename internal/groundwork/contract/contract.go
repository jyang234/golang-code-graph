// Package contract decodes flowmap's gated boundary contract (the output of
// `flowmap boundary <service>`) and computes the semantic diff between a base and
// a branch contract — the inter-service surface movement that `groundwork diff`
// reports and flags as breaking.
//
// Unlike the graph decode (which must track flowmap's schema exactly because it
// feeds a digest), this decode is deliberately lenient: unknown fields are
// ignored, so a future flowmap field does not break the diff. The diff is a
// semantic comparison, not a byte-exact reproduction.
package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Contract is the subset of flowmap's boundary contract that the diff compares:
// the service's inbound HTTP routes and consumed events, the events it publishes,
// and its outbound dependencies. Blind spots and schema version are ignored here.
type Contract struct {
	Service      string        `json:"service"`
	EntryPoints  EntryPoints   `json:"entrypoints"`
	Published    []Event       `json:"published"`
	Consumed     []Event       `json:"consumed"`
	ExternalDeps []ExternalDep `json:"external_dependencies"`
}

// EntryPoints is the inbound surface.
type EntryPoints struct {
	HTTP      []HTTPEntry `json:"http"`
	Consumers []Event     `json:"consumers"`
}

// HTTPEntry is one inbound route.
type HTTPEntry struct {
	Method string `json:"method"`
	Route  string `json:"route"`
	Tier   int    `json:"tier"`
}

// Event is a published, consumed, or consumer-entrypoint event.
type Event struct {
	Event string `json:"event"`
	Tier  int    `json:"tier"`
}

// ExternalDep is one outbound dependency on another service.
type ExternalDep struct {
	Peer string   `json:"peer"`
	Kind string   `json:"kind"`
	Ops  []string `json:"ops"`
	Tier int      `json:"tier"`
}

// Load decodes a boundary contract from JSON, tolerating unknown fields.
func Load(path string) (*Contract, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Contract
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("groundwork/contract: %s: %w", path, err)
	}
	// A real boundary contract always names its service. An empty ("{}"), null,
	// or truncated file decodes to a zero Contract without error, and Compare
	// would then read every base route/event as a breaking removal — a spurious
	// BLOCK attributed to a contract change that is really just a malformed input.
	// Fail closed on the missing service name instead.
	if strings.TrimSpace(c.Service) == "" {
		return nil, fmt.Errorf("groundwork/contract: %s: missing service name (empty or malformed contract)", path)
	}
	return &c, nil
}

// Change is one movement of the inter-service surface.
type Change struct {
	Op       string `json:"op"`      // "+" added, "-" removed, "~" changed
	Surface  string `json:"surface"` // route | publish | consume | dependency
	Name     string `json:"name"`
	Breaking bool   `json:"breaking,omitempty"`
}

// Diff is the full set of changes between two contracts.
type Diff struct {
	Service string   `json:"service"`
	Changes []Change `json:"changes"`
}

// Empty reports whether the contracts are identical on the compared surfaces.
func (d Diff) Empty() bool { return len(d.Changes) == 0 }

// Breaking reports whether any change removes a promise the service makes to
// others (a route, a published event, or a consumed event it no longer handles).
func (d Diff) Breaking() bool {
	for _, c := range d.Changes {
		if c.Breaking {
			return true
		}
	}
	return false
}

// Compare computes the base→branch contract diff. Removals of routes, published
// events, and consumed events are breaking (a downstream service can break);
// additions are not. Outbound dependency changes are reported but never breaking —
// they are the service's own requirements, not promises to others.
func Compare(base, branch *Contract) Diff {
	d := Diff{Service: branch.Service}

	d.Changes = append(d.Changes, diffSet("route", routeKeys(base), routeKeys(branch), true)...)
	d.Changes = append(d.Changes, diffSet("publish", eventKeys(base.Published), eventKeys(branch.Published), true)...)
	d.Changes = append(d.Changes, diffSet("consume", eventKeys(base.Consumed), eventKeys(branch.Consumed), true)...)
	d.Changes = append(d.Changes, diffDeps(base.ExternalDeps, branch.ExternalDeps)...)

	sort.SliceStable(d.Changes, func(i, j int) bool {
		a, b := d.Changes[i], d.Changes[j]
		if a.Surface != b.Surface {
			return a.Surface < b.Surface
		}
		if a.Op != b.Op {
			return a.Op < b.Op
		}
		return a.Name < b.Name
	})
	return d
}

// diffSet emits +/- changes between two key sets; removalBreaking marks removals
// as breaking.
func diffSet(surface string, base, branch map[string]bool, removalBreaking bool) []Change {
	var out []Change
	for k := range branch {
		if !base[k] {
			out = append(out, Change{Op: "+", Surface: surface, Name: k})
		}
	}
	for k := range base {
		if !branch[k] {
			out = append(out, Change{Op: "-", Surface: surface, Name: k, Breaking: removalBreaking})
		}
	}
	return out
}

// diffDeps emits +/-/~ changes for outbound dependencies (never breaking).
func diffDeps(base, branch []ExternalDep) []Change {
	b := depMap(base)
	h := depMap(branch)
	var out []Change
	for key, hd := range h {
		bd, ok := b[key]
		switch {
		case !ok:
			out = append(out, Change{Op: "+", Surface: "dependency", Name: hd.peer})
		case bd.sig != hd.sig:
			out = append(out, Change{Op: "~", Surface: "dependency", Name: hd.peer})
		}
	}
	for key, bd := range b {
		if _, ok := h[key]; !ok {
			out = append(out, Change{Op: "-", Surface: "dependency", Name: bd.peer})
		}
	}
	return out
}

func routeKeys(c *Contract) map[string]bool {
	m := make(map[string]bool, len(c.EntryPoints.HTTP))
	for _, e := range c.EntryPoints.HTTP {
		m[e.Method+" "+e.Route] = true
	}
	return m
}

func eventKeys(es []Event) map[string]bool {
	m := make(map[string]bool, len(es))
	for _, e := range es {
		m[e.Event] = true
	}
	return m
}

// depSig is a dependency's diff identity: the bare peer (for the human-facing change
// name) plus a stable op signature (so a changed op set is detectable).
type depSig struct {
	peer string
	sig  string
}

// depMap keys dependencies by peer AND kind, so one peer that is reached by two
// kinds (e.g. an HTTP call and an object-store call to the same package) does not
// collapse to a single entry and silently hide one kind's surface movement from the
// diff. Keying by peer alone dropped the second kind.
func depMap(deps []ExternalDep) map[string]depSig {
	m := make(map[string]depSig, len(deps))
	for _, d := range deps {
		ops := append([]string(nil), d.Ops...)
		sort.Strings(ops)
		m[d.Peer+"\x00"+d.Kind] = depSig{peer: d.Peer, sig: d.Kind + ":" + strings.Join(ops, ",")}
	}
	return m
}
