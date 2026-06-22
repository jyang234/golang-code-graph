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

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
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
		if k := opkey.PublishPrefix + e.Event; !seen[k] {
			out = append(out, Effect{Publish, k, e.Tier})
		}
	}
	for _, e := range c.Consumed {
		if k := opkey.ConsumePrefix + e.Event; !seen[k] {
			out = append(out, Effect{Consume, k, e.Tier})
		}
	}
	for _, d := range c.ExternalDeps {
		for _, op := range d.Ops {
			// Skip kinds with no behavioral op vocabulary (object storage, cache):
			// externalKey cannot produce a key any collected span would carry, so
			// counting them would report a forever-Unexercised effect no flow could
			// ever cover — noise, not a measurement. Honest coverage only measures
			// surfaces it can match.
			k, ok := externalKey(d, op)
			if !ok {
				continue
			}
			if !seen[k] {
				out = append(out, Effect{External, k, d.Tier})
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return lessEffect(out[i], out[j]) })
	return Report{Unexercised: out}
}

// lessEffect is the canonical order over unexercised effects: most consequential
// (lowest tier) first, then by op key, then by category. Category is a genuine
// tie-break, not decoration: keys are namespaced by verb prefix (PUBLISH/CONSUME/
// HTTP/…) so a cross-category Tier+Key collision is rare, but including it makes
// the order a TOTAL function of intrinsic fields rather than resting on the
// build-loop append order (CLAUDE.md: break every tie deterministically).
func lessEffect(a, b Effect) bool {
	if a.Tier != b.Tier {
		return a.Tier < b.Tier
	}
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	return a.Category < b.Category
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

// externalKey renders a boundary external-dependency op into the canonical key a
// behavioral client span carries, plus ok=false for a kind that has NO behavioral
// vocabulary (object storage / cache today) — for which no span key exists, so the
// caller must skip it rather than emit a forever-unmatchable key.
func externalKey(d boundary.ExternalDep, op string) (string, bool) {
	switch d.Kind {
	case "http":
		method, route, _ := strings.Cut(op, " ")
		parts := []string{"HTTP", method, d.Peer}
		if route != "" {
			parts = append(parts, route)
		}
		return strings.Join(parts, " "), true
	case "rpc", "grpc":
		return opkey.RPCPrefix + op, true
	default:
		return "", false // no behavioral op vocabulary for this kind (e.g. blob, cache)
	}
}
