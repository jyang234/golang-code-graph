// Package syscontext aggregates many post-hoc flows into one deduplicated
// service-interaction graph — the system-context view (post-hoc design: the
// merged diagram unit). Where a per-flow diagram is a sequence, this is a
// graph: nodes are services and the infrastructure they touch (the broker,
// databases, external peers), edges are the interactions (HTTP/RPC calls,
// DB access, publish/consume), deduplicated across every captured flow.
//
// It optionally overlays the static boundary contracts: an interaction the code
// can do but no captured flow exercised is added as a dashed edge — a
// system-level coverage view ("here is the architecture, and here is the part
// your tests never touched"). The static side is passed in as neutral data, so
// this package depends only on ir, not on the static analyzer.
//
// The graph is a pure function of its inputs: nodes and edges are sorted and
// deduplicated, so re-running the e2e suite never churns it.
//
// Node identity is the lifeline name string: a service.name (callee side) and a
// peer.service / server.address (caller side) coincide into one node only when
// they are byte-equal. If a caller names a dependency differently from the
// callee's own resource service.name, that one service appears as two nodes
// (an external peer with the inbound edge and a service with its downstream
// edges). Keep peer.service aligned with the callee's service.name across your
// instrumentation, or expect a split.
package syscontext

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/ir"
)

// NodeKind classifies a lifeline for rendering.
type NodeKind string

const (
	KindService  NodeKind = "service"  // owns spans (first-party)
	KindBroker   NodeKind = "broker"   // the event bus
	KindExternal NodeKind = "external" // a peer/DB that never owns spans here
	KindClient   NodeKind = "client"   // the synthetic ingress caller
)

// Node is one lifeline in the system graph.
type Node struct {
	Name string
	Kind NodeKind
}

// Edge is one deduplicated interaction. Dashed means it is backed only by the
// static contract (can happen) and was not exercised by any captured flow.
type Edge struct {
	From   string
	To     string
	Label  string
	Dashed bool
}

// Graph is the deduplicated, sorted system-context graph.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// Dep is a static external dependency (neutral form of boundary.ExternalDep).
type Dep struct {
	Peer string
	Kind string // "http" | "rpc" | …
}

// Contract is the neutral form of a static boundary contract for overlay.
type Contract struct {
	Service   string
	Published []string
	Consumed  []string
	Deps      []Dep
}

// Options tunes the build.
type Options struct {
	// Choreography draws publisher→subscriber edges joined on the event name
	// (the event-coupling view) instead of routing through a visible Bus node.
	Choreography bool
}

const busNode = "Bus"

// Build aggregates runtime traces and (optional) static contracts into one graph.
func Build(traces []*ir.CanonicalTrace, statics []Contract, opt Options) *Graph {
	b := newBuilder(opt.Choreography)
	for _, t := range traces {
		b.addTrace(t)
	}
	for _, c := range statics {
		b.addContract(c)
	}
	return b.graph()
}

type builder struct {
	choreography bool
	ranks        map[string]int             // node name -> kind rank (highest wins)
	edges        map[string]*Edge           // key -> edge
	pub          map[string]map[string]bool // event -> service -> runtime-solid
	con          map[string]map[string]bool // event -> service -> runtime-solid
}

func newBuilder(choreography bool) *builder {
	return &builder{
		choreography: choreography,
		ranks:        map[string]int{},
		edges:        map[string]*Edge{},
		pub:          map[string]map[string]bool{},
		con:          map[string]map[string]bool{},
	}
}

func (b *builder) addTrace(t *ir.CanonicalTrace) {
	if t == nil || t.Root == nil {
		return
	}
	b.walk(t.Root, "", t.Service, true)
}

// walk visits a span carrying parent — the service that owns the enclosing span,
// which is the lifeline the inbound hop comes from.
func (b *builder) walk(s *ir.CanonicalSpan, parent, fallback string, solid bool) {
	svc := lifeline(s.Service, fallback)
	switch s.Kind {
	case ir.KindClient:
		if s.Peer != "" {
			b.node(svc, KindService)
			b.node(s.Peer, KindExternal)
			b.edge(svc, s.Peer, clientLabel(s.Op), solid)
		}
	case ir.KindProducer:
		b.node(svc, KindService)
		b.publish(strings.TrimPrefix(s.Op, opkey.PublishPrefix), svc, solid)
	case ir.KindConsumer:
		b.node(svc, KindService)
		b.consume(strings.TrimPrefix(s.Op, opkey.ConsumePrefix), svc, solid)
	case ir.KindServer:
		b.node(svc, KindService)
		switch {
		case parent == "":
			// An ingress with no in-flow caller (the entry, or a multi-entry
			// flow's entries).
			b.node("Client", KindClient)
			b.edge("Client", svc, "", solid)
		case parent != svc:
			// A callee entry: recover the cross-service hop from the tree nesting
			// rather than the caller's client span, whose peer.service may differ
			// from this service's service.name (or may be absent from the capture).
			b.edge(parent, svc, "", solid)
		}
	}
	// Children are owned by this span's service when it has one; a service-less
	// node (a synthesized root) passes the parent through unchanged.
	childParent := parent
	if s.Service != "" {
		childParent = svc
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			b.walk(m, childParent, fallback, solid)
		}
	}
}

func (b *builder) addContract(c Contract) {
	if c.Service == "" {
		return
	}
	b.node(c.Service, KindService)
	for _, d := range c.Deps {
		if d.Peer == "" {
			continue
		}
		b.node(d.Peer, KindExternal)
		b.edge(c.Service, d.Peer, depLabel(d.Kind), false) // static-only ⇒ dashed unless a runtime edge upgrades it
	}
	for _, e := range c.Published {
		b.publish(e, c.Service, false)
	}
	for _, e := range c.Consumed {
		b.consume(e, c.Service, false)
	}
}

func (b *builder) publish(event, svc string, solid bool) {
	if event == "" {
		return
	}
	mark(b.pub, event, svc, solid)
	if !b.choreography {
		b.node(busNode, KindBroker)
		b.edge(svc, busNode, "publish "+event, solid)
	}
}

func (b *builder) consume(event, svc string, solid bool) {
	if event == "" {
		return
	}
	mark(b.con, event, svc, solid)
	if !b.choreography {
		b.node(busNode, KindBroker)
		b.edge(busNode, svc, event, solid)
	}
}

func (b *builder) graph() *Graph {
	// Choreography: join publishers to consumers on the event name. The edge is
	// solid only when both ends are runtime-exercised.
	if b.choreography {
		for event, pubs := range b.pub {
			for p, pSolid := range pubs {
				for c, cSolid := range b.con[event] {
					b.edge(p, c, event, pSolid && cSolid)
				}
			}
		}
	}

	g := &Graph{}
	for name, rank := range b.ranks {
		g.Nodes = append(g.Nodes, Node{Name: name, Kind: kindOfRank(rank)})
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Name < g.Nodes[j].Name })
	for _, e := range b.edges {
		g.Edges = append(g.Edges, *e)
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Label < g.Edges[j].Label
	})
	return g
}

func (b *builder) node(name string, kind NodeKind) {
	if name == "" {
		return
	}
	r := rankOf(kind)
	if cur, ok := b.ranks[name]; !ok || r > cur {
		b.ranks[name] = r
	}
}

func (b *builder) edge(from, to, label string, solid bool) {
	if from == "" || to == "" {
		return
	}
	k := from + "\x00" + to + "\x00" + label
	e := b.edges[k]
	if e == nil {
		e = &Edge{From: from, To: to, Label: label, Dashed: !solid}
		b.edges[k] = e
	}
	if solid {
		e.Dashed = false
	}
}

func mark(m map[string]map[string]bool, event, svc string, solid bool) {
	if m[event] == nil {
		m[event] = map[string]bool{}
	}
	if solid || !m[event][svc] {
		// runtime evidence (solid) wins; otherwise keep/initialize as static-only.
		m[event][svc] = m[event][svc] || solid
	}
}

func lifeline(name, fallback string) string {
	if name != "" {
		return name
	}
	if fallback != "" {
		return fallback
	}
	return "service"
}

func clientLabel(op string) string {
	switch {
	case strings.HasPrefix(op, opkey.DBPrefix):
		return "DB"
	case strings.HasPrefix(op, opkey.RPCPrefix):
		return "RPC"
	default:
		return "HTTP"
	}
}

func depLabel(kind string) string {
	switch kind {
	case "rpc", "grpc":
		return "RPC"
	case "", "http":
		return "HTTP"
	default:
		return strings.ToUpper(kind)
	}
}

func rankOf(k NodeKind) int {
	switch k {
	case KindService:
		return 3
	case KindBroker:
		return 2
	case KindExternal:
		return 1
	default:
		return 0
	}
}

func kindOfRank(r int) NodeKind {
	switch r {
	case 3:
		return KindService
	case 2:
		return KindBroker
	case 1:
		return KindExternal
	default:
		return KindClient
	}
}
