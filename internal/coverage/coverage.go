// Package coverage computes flowmap's emergent capability: the delta between the
// static inter-service boundary (every statically-reachable effect) and the union
// of behavioral snapshots (the effects tested flows actually exercised). What
// remains is contract-level coverage feedback — "you publish loan.declined on a
// path no flow drives" — which neither pipeline produces alone (scope &
// guarantees §3).
//
// The join is over one key space (plan [H2]): a boundary effect and a behavioral
// span map to the same canonical op key — a published event loan.approved and a
// PUBLISH loan.approved span; an external dependency (credit-bureau, GET,
// /score/{id}) and an HTTP GET credit-bureau /score/{id} span. The unexercised
// set is the boundary keys absent from the union of behavioral op keys.
package coverage

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/static/boundary"
	"github.com/jyang234/golang-code-graph/ir"
)

// Category names which kind of boundary effect went unexercised.
type Category string

const (
	Publish  Category = "publish"
	Consume  Category = "consume"
	External Category = "external"
)

// Effect is one boundary effect with no behavioral evidence.
type Effect struct {
	Category Category
	Key      string // the canonical op key, e.g. "PUBLISH loan.declined"
	Tier     int
}

// Report is the coverage delta: the boundary effects no flow exercised.
type Report struct {
	Unexercised []Effect
}

// Empty reports whether every boundary effect is exercised by some flow.
func (r Report) Empty() bool { return len(r.Unexercised) == 0 }

// Delta returns the boundary effects (published/consumed events and external
// dependencies) that the union of traces does not exercise. DB operations and
// entry points are out of scope: the boundary contract excludes the former, and
// the join is specified over outbound/consumed effects (plan [H2]).
func Delta(c *boundary.Contract, traces []*ir.CanonicalTrace) Report {
	if c == nil {
		return Report{}
	}
	seen := map[string]bool{}
	for _, t := range traces {
		if t != nil {
			collectOps(t.Root, seen)
		}
	}

	var out []Effect
	for _, e := range c.Published {
		if k := "PUBLISH " + e.Event; !seen[k] {
			out = append(out, Effect{Publish, k, e.Tier})
		}
	}
	for _, e := range c.Consumed {
		if k := "CONSUME " + e.Event; !seen[k] {
			out = append(out, Effect{Consume, k, e.Tier})
		}
	}
	for _, d := range c.ExternalDeps {
		for _, op := range d.Ops {
			if k := externalKey(d, op); !seen[k] {
				out = append(out, Effect{External, k, d.Tier})
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier // most consequential first
		}
		return out[i].Key < out[j].Key
	})
	return Report{Unexercised: out}
}

// collectOps records every span's canonical op key in the trace subtree.
func collectOps(s *ir.CanonicalSpan, into map[string]bool) {
	if s == nil {
		return
	}
	into[s.Op] = true
	for _, g := range s.Children {
		for _, m := range g.Members {
			collectOps(m, into)
		}
	}
}

// externalKey renders a boundary external-dependency op into the same canonical
// key a behavioral client span carries. An HTTP dependency stores its op as
// "<METHOD> <route>"; the behavioral span keys it as "HTTP <METHOD> <peer>
// <route>", so the peer is spliced in after the method.
func externalKey(d boundary.ExternalDep, op string) string {
	switch d.Kind {
	case "http":
		method, route, _ := strings.Cut(op, " ")
		parts := []string{"HTTP", method, d.Peer}
		if route != "" {
			parts = append(parts, route)
		}
		return strings.Join(parts, " ")
	case "rpc", "grpc":
		return "RPC " + op
	default:
		return strings.ToUpper(d.Kind) + " " + d.Peer + " " + op
	}
}
