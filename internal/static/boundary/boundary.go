// Package boundary derives flowmap's gated static artifact: the inter-service
// boundary contract (static-extractor spec §4). It is what the service exposes,
// consumes, publishes, and calls externally — the surface stable under internal
// refactoring — plus the boundary blind-spot manifest. It deliberately excludes
// DB operations (the database is the service's own store, owned by the behavioral
// snapshot) and internal call structure.
//
// The contract is exhaustive only over statically-resolvable paths: a publish or
// outbound call with a non-constant target cannot be named, so it is recorded in
// the blind-spot manifest instead of silently omitted. The whole artifact is
// canonical JSON — sorted, position-insensitive — so regenerating it yields
// byte-identical output and the currency gate diffs only on a genuine boundary
// change.
package boundary

import (
	"sort"
	"strings"

	"golang.org/x/tools/go/ssa"

	"github.com/jyang234/golang-code-graph/internal/canonjson"
	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
	"github.com/jyang234/golang-code-graph/internal/static/features"
	"github.com/jyang234/golang-code-graph/internal/static/roots"
)

// SchemaVersion identifies the contract's canonical form. A flowmap change to
// that form bumps it and requires coordinated regeneration — the real blast
// radius, made explicit rather than silent.
const SchemaVersion = "flowmap.boundary/v1"

// Contract is the gated inter-service boundary contract.
type Contract struct {
	Service       string                 `json:"service"`
	SchemaVersion string                 `json:"schema_version"`
	EntryPoints   EntryPoints            `json:"entrypoints"`
	Published     []Event                `json:"published"`
	Consumed      []Event                `json:"consumed"`
	ExternalDeps  []ExternalDep          `json:"external_dependencies"`
	BlindSpots    []blindspots.BlindSpot `json:"blind_spots"`
}

// EntryPoints are the service's exposed HTTP routes and bus consumers.
type EntryPoints struct {
	HTTP      []HTTPEntry `json:"http"`
	Consumers []Event     `json:"consumers"`
}

// HTTPEntry is one exposed HTTP route.
type HTTPEntry struct {
	Method string `json:"method"`
	Route  string `json:"route"`
	Tier   int    `json:"tier"`
}

// Event is a published or consumed event name with its tier.
type Event struct {
	Event string `json:"event"`
	Tier  int    `json:"tier"`
}

// ExternalDep is a downstream service dependency and the operations called on it.
type ExternalDep struct {
	Peer string   `json:"peer"`
	Kind string   `json:"kind"`
	Ops  []string `json:"ops"`
	Tier int      `json:"tier"`
}

// Extract derives the contract from an analyzed service unit.
func Extract(res *analyze.Result) *Contract {
	ext := features.NewExtractor(res.Config, res.Program.ModulePath)
	hints := ext.Hints()

	c := &Contract{
		Service:       res.ServiceName(),
		SchemaVersion: SchemaVersion,
	}

	// Entry points and consumed events come from the discovered roots.
	for _, r := range res.Roots.Roots {
		switch r.Kind {
		case roots.KindHTTP:
			method, route := splitRoute(r.Name)
			tier, _ := ext.Classify(ext.Inbound(r.Name, false))
			c.EntryPoints.HTTP = append(c.EntryPoints.HTTP, HTTPEntry{Method: method, Route: route, Tier: tier})
		case roots.KindConsumer:
			tier, _ := ext.Classify(ext.Inbound(r.Name, false))
			c.EntryPoints.Consumers = append(c.EntryPoints.Consumers, Event{Event: r.Name, Tier: tier})
			c.Consumed = append(c.Consumed, Event{Event: r.Name, Tier: tier})
			// KindCallback / KindWorker (declared roots) are intentionally not added
			// to the named-event surface: their Name is a config FQN, not a route or
			// event, so listing it would pollute the gated contract with a non-event.
			// Their VALUE is that rooting them makes their effect cone reachable, so the
			// publishes and external deps they reach DO enter the contract below through
			// the normal reachable-node traversal — the recovered boundary surface,
			// without a fabricated entry name. (Their DB writes become reachable in the
			// GRAPH, not in this contract, which excludes DB by design — see below.)
		}
	}

	// Published events and external dependencies come from reachable first-party
	// boundary calls with constant targets. DB calls are intentionally ignored.
	published := make(map[string]int)
	deps := make(map[string]*ExternalDep)
	for _, n := range res.Graph.Nodes {
		if !res.Program.IsFirstParty(n.Func.Pkg) {
			continue
		}
		for _, e := range n.Out {
			callee := e.Callee.Func
			switch {
			case hints.IsPublish(callee):
				if event, ok := constEvent(e.Site); ok {
					tier, _ := ext.Classify(ext.Published(event))
					published[event] = tier
				}
			case hints.IsHTTP(callee):
				if peer, method, route, ok := constHTTP(e.Site); ok {
					op := method + " " + route
					tier, _ := ext.Classify(ext.External(peer + " " + op))
					key := peer + "|http"
					d := deps[key]
					if d == nil {
						d = &ExternalDep{Peer: peer, Kind: "http", Tier: tier}
						deps[key] = d
					}
					d.Ops = appendUnique(d.Ops, op)
					if tier < d.Tier {
						d.Tier = tier // surface the most consequential op's tier
					}
				}
			case features.IsPackageInit(callee):
				// A synthesized package initializer matched a bare-package classify hint
				// (init-ordering plumbing, not an operation). edgeOf guards this for the
				// graph view; the contract's own loop must too, or a bare hint would
				// list "init" as an operation.
			default:
				// A method-named outbound effect (blob/cache/rpc) is an external
				// dependency like an HTTP peer, so it belongs in the gated contract (not
				// dropped). It carries no const peer/op triple — the peer is the client
				// package and the op is the method name — so it is named without a
				// constant-arg guard. This is what keeps promoting such a call from a
				// blind spot to a typed kind from silently losing its gated disclosure.
				if kind, ok := hints.MethodNamedOutboundKind(callee); ok {
					peer := features.PkgPath(callee)
					op := callee.Name()
					tier, _ := ext.Classify(ext.External(peer + " " + op))
					key := peer + "|" + kind
					d := deps[key]
					if d == nil {
						d = &ExternalDep{Peer: peer, Kind: kind, Tier: tier}
						deps[key] = d
					}
					d.Ops = appendUnique(d.Ops, op)
					if tier < d.Tier {
						d.Tier = tier
					}
				}
			}
		}
	}
	for event, tier := range published {
		c.Published = append(c.Published, Event{Event: event, Tier: tier})
	}
	for _, d := range deps {
		c.ExternalDeps = append(c.ExternalDeps, *d)
	}

	// Only the boundary subset gates; the graph-completeness disclosures (reflect,
	// fan-out, unsafe/cgo/linkname) ride the non-gated graph view, so an internal
	// refactor never churns the contract (static-extractor §7).
	c.BlindSpots = blindspots.Boundary(blindspots.Detect(res, hints))
	c.normalize()
	return c
}

// Marshal renders the contract as canonical, gated JSON.
func (c *Contract) Marshal() ([]byte, error) { return canonjson.Marshal(c) }

// normalize sorts every collection and replaces nil slices with empty ones, so
// two extractions of the same program are byte-identical and the JSON shape is
// stable across services.
func (c *Contract) normalize() {
	if c.EntryPoints.HTTP == nil {
		c.EntryPoints.HTTP = []HTTPEntry{}
	}
	if c.EntryPoints.Consumers == nil {
		c.EntryPoints.Consumers = []Event{}
	}
	if c.Published == nil {
		c.Published = []Event{}
	}
	if c.Consumed == nil {
		c.Consumed = []Event{}
	}
	if c.ExternalDeps == nil {
		c.ExternalDeps = []ExternalDep{}
	}
	if c.BlindSpots == nil {
		c.BlindSpots = []blindspots.BlindSpot{}
	}
	sort.Slice(c.EntryPoints.HTTP, func(i, j int) bool {
		a, b := c.EntryPoints.HTTP[i], c.EntryPoints.HTTP[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		return a.Method < b.Method
	})
	sortEvents(c.EntryPoints.Consumers)
	sortEvents(c.Published)
	sortEvents(c.Consumed)
	sort.Slice(c.ExternalDeps, func(i, j int) bool {
		a, b := c.ExternalDeps[i], c.ExternalDeps[j]
		if a.Peer != b.Peer {
			return a.Peer < b.Peer
		}
		return a.Kind < b.Kind
	})
	for i := range c.ExternalDeps {
		sort.Strings(c.ExternalDeps[i].Ops)
	}
}

func sortEvents(es []Event) {
	sort.Slice(es, func(i, j int) bool { return es[i].Event < es[j].Event })
}

// splitRoute splits a root name like "POST /loan-application" into method and
// route; a name with no space (a topic) yields an empty method.
func splitRoute(name string) (method, route string) {
	if i := strings.IndexByte(name, ' '); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

func constEvent(site ssa.CallInstruction) (string, bool) {
	args := features.StringArgs(site)
	if len(args) < 1 {
		return "", false
	}
	return features.ConstString(args[0])
}

func constHTTP(site ssa.CallInstruction) (peer, method, route string, ok bool) {
	args := features.StringArgs(site)
	if len(args) < 3 {
		return "", "", "", false
	}
	p, ok1 := features.ConstString(args[0])
	m, ok2 := features.ConstString(args[1])
	r, ok3 := features.ConstString(args[2])
	if !ok1 || !ok2 || !ok3 {
		return "", "", "", false
	}
	return p, m, r, true
}

func appendUnique(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(ss, s)
}
