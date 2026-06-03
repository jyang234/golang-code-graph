// Package render turns the canonical IR into a Mermaid sequence diagram — the
// human-readable view committed alongside the golden for review (canon spec §3.8,
// golden-diff spec). The IR is the gated assertion; the diagram is a deterministic
// function of it, so renderer drift never pollutes the gate.
//
// The diagram is rendered from one service's perspective: every message
// originates at the self lifeline. Concurrent child groups become par/and blocks
// (never alt — the IR has no branches; error flows are separate goldens), loop
// collapse becomes a multiplicity note, and participant order is fixed
// (caller, self, then peers sorted) so the output is byte-stable.
package render

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/syscontext"
	"github.com/jyang234/golang-code-graph/ir"
)

// SystemGraph renders a deduplicated system-context graph (services + the
// infrastructure they touch) as a Mermaid flowchart. Solid edges were exercised
// by a captured flow; dashed edges come only from the static contract overlay
// (can happen, untested). Node shapes distinguish the kinds: services are
// rectangles, the broker a hexagon, external peers/DBs stadiums.
func SystemGraph(g *syscontext.Graph) string {
	var b strings.Builder
	b.WriteString("graph LR\n")
	alias := make(map[string]string, len(g.Nodes))
	used := map[string]bool{}
	for _, n := range g.Nodes {
		id := uniqueID(sanitize(n.Name), used)
		alias[n.Name] = id
		open, close := nodeShape(n.Kind)
		b.WriteString("    " + id + open + `"` + n.Name + `"` + close + "\n")
	}
	for _, e := range g.Edges {
		arrow := "-->"
		if e.Dashed {
			arrow = "-.->"
		}
		line := "    " + alias[e.From] + " " + arrow
		if e.Label != "" {
			line += "|\"" + e.Label + "\"|"
		}
		line += " " + alias[e.To] + "\n"
		b.WriteString(line)
	}
	return b.String()
}

func nodeShape(k syscontext.NodeKind) (open, close string) {
	switch k {
	case syscontext.KindBroker:
		return "{{", "}}"
	case syscontext.KindExternal:
		return "([", "])"
	default: // service, client
		return "[", "]"
	}
}

// Mermaid renders t as a Mermaid sequenceDiagram. The output ends with a newline
// and is a pure, deterministic function of the IR.
func Mermaid(t *ir.CanonicalTrace) string {
	r := newRenderer()
	r.self = lifelineLabel(t.Service, "service")
	r.caller = callerLabel(t.Root)

	var b strings.Builder
	b.WriteString("sequenceDiagram\n")
	r.writeParticipants(&b, t)
	if t.Root != nil {
		b.WriteString("    " + r.msg(r.caller, r.self, label(t.Root)))
		r.writeGroups(&b, t.Root.Children, "    ")
	}
	return b.String()
}

type renderer struct {
	self   string
	caller string
	alias  map[string]string // lifeline label -> mermaid-safe id
	used   map[string]bool   // taken ids, for unique-id assignment
	// ref records the lifelines an arrow or note actually drew to. The cross-service
	// renderer declares only these, pruning over-declared participants no edge
	// touches. The per-service Mermaid renderer declares its fixed caller/self/peer
	// set up front and draws to each, so it reads nothing from ref.
	ref map[string]bool
}

// newRenderer returns a renderer with its maps initialized.
func newRenderer() *renderer {
	return &renderer{
		alias: map[string]string{},
		used:  map[string]bool{},
		ref:   map[string]bool{},
	}
}

// aliasOf returns the Mermaid-safe id for a lifeline, assigning a unique one on
// first sight. It records nothing about whether the lifeline is drawn — callers
// that mean "this lifeline is touched by an edge" use id instead.
func (r *renderer) aliasOf(name string) string {
	if id, ok := r.alias[name]; ok {
		return id
	}
	id := uniqueID(sanitize(name), r.used)
	r.alias[name] = id
	return id
}

// SystemMermaid renders a whole-flow, CROSS-SERVICE sequence diagram from a trace
// whose spans carry per-span Service (an out-of-process whole-flow capture). Where
// Mermaid pins one self lifeline, this switches lifelines per span so the diagram
// shows every service the flow touched and the hops between them — what a service
// interacts with end to end (post-hoc design: the diagram unit is the whole
// flow, not the per-service gate fragment). It is a view, never gated.
func SystemMermaid(t *ir.CanonicalTrace) string {
	if t.Root == nil {
		return "sequenceDiagram\n"
	}
	return systemMermaidCore(callerLabel(t.Root), t.Root, t.Service)
}

// SystemMermaidRootedAt renders the cross-service view centered on one service:
// the subtree(s) the service owns — everything it does and reaches downstream —
// with the lifeline that called into it as the caller. It returns ok=false if the
// service does not appear in the flow. A service entered more than once in the
// flow gets its entries gathered under a synthetic root.
func SystemMermaidRootedAt(t *ir.CanonicalTrace, service string) (string, bool) {
	var entries []*ir.CanonicalSpan
	var callers []string
	var walk func(n *ir.CanonicalSpan, parentSvc string)
	walk = func(n *ir.CanonicalSpan, parentSvc string) {
		if n == nil {
			return
		}
		if n.Service == service && parentSvc != service {
			entries = append(entries, n) // its whole subtree (incl. nested re-entries) renders together
			callers = append(callers, parentSvc)
			return
		}
		for _, g := range n.Children {
			for _, m := range g.Members {
				walk(m, n.Service)
			}
		}
	}
	walk(t.Root, "")

	switch len(entries) {
	case 0:
		return "", false
	case 1:
		caller := callers[0]
		if caller == "" {
			caller = callerLabel(entries[0])
		}
		return systemMermaidCore(caller, entries[0], t.Service), true
	default:
		syn := &ir.CanonicalSpan{
			Op: service, Kind: ir.KindServer, Service: service,
			Children: []ir.ChildGroup{{Concurrent: true, Members: entries}},
		}
		return systemMermaidCore("Client", syn, t.Service), true
	}
}

// systemMermaidCore renders the cross-service sequence diagram for one subtree,
// from an explicit caller lifeline. fallback is the service to attribute spans
// that carry no service.name.
func systemMermaidCore(caller string, root *ir.CanonicalSpan, fallback string) string {
	r := newRenderer()
	entry := landingOf(root, fallback)

	// Plan the participant layout family-adjacent: the synthetic caller, then each
	// service boxed with the databases it exclusively owns (a store reached only from
	// that service), then — appended after the body is rendered — the shared lifelines
	// the body actually drew to (the broker, external peers, multi-owner databases),
	// sorted. This keeps a service and its infrastructure together instead of
	// interleaving every service's databases alphabetically. Aliases are assigned as
	// the plan is built (so an id is stable); declarations are emitted only for the
	// lifelines r.ref says an arrow or note touched, so an over-declared peer is
	// pruned rather than drawn as a bare, dangling line.
	services, ownedDB := serviceInfra(root, fallback)
	plan := newParticipantPlan(r)
	plan.loose(caller)                  // the ingress/bus, never boxed with a service
	plan.service(entry, ownedDB[entry]) // the entry service leads, boxed with its dbs
	svcs := make([]string, 0, len(services))
	for s := range services {
		if s != entry && s != caller {
			svcs = append(svcs, s)
		}
	}
	sort.Strings(svcs)
	for _, svc := range svcs {
		plan.service(svc, ownedDB[svc])
	}

	// Render the body; r.id records every lifeline an arrow or note touches and
	// assigns aliases to peers met along the way.
	var body strings.Builder
	body.WriteString("    " + r.msg(caller, entry, label(root)))
	r.writeSystemGroups(&body, root.Children, entry, fallback, "    ")

	// Shared lifelines the body drew to that the plan didn't already place, sorted.
	rest := make([]string, 0, len(r.ref))
	for name := range r.ref {
		if !plan.placed[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		plan.loose(name)
	}

	var b strings.Builder
	b.WriteString("sequenceDiagram\n")
	plan.declare(&b, r.ref)
	b.WriteString(body.String())
	return b.String()
}

// participantPlan accumulates the ordered participant layout — loose lifelines and
// service boxes — and the aliases for each, so declarations can be emitted after the
// body has revealed which lifelines an edge touches.
type participantPlan struct {
	r      *renderer
	blocks []participantBlock
	placed map[string]bool // names already planned (deduped)
}

// participantBlock is one unit of the layout: a loose participant (box == "") or a
// service boxed with the databases it owns (names == [service, dbs…]).
type participantBlock struct {
	box   string
	names []string
}

func newParticipantPlan(r *renderer) *participantPlan {
	return &participantPlan{r: r, placed: map[string]bool{}}
}

// loose plans one standalone participant.
func (p *participantPlan) loose(name string) {
	if name == "" || p.placed[name] {
		return
	}
	p.placed[name] = true
	p.r.aliasOf(name)
	p.blocks = append(p.blocks, participantBlock{names: []string{name}})
}

// service plans a service boxed with the databases it exclusively owns, or — when it
// owns none — as a loose participant.
func (p *participantPlan) service(svc string, ownedDB []string) {
	if svc == "" || p.placed[svc] {
		return
	}
	if len(ownedDB) == 0 {
		p.loose(svc)
		return
	}
	p.placed[svc] = true
	p.r.aliasOf(svc)
	names := []string{svc}
	for _, db := range ownedDB {
		p.placed[db] = true
		p.r.aliasOf(db)
		names = append(names, db)
	}
	p.blocks = append(p.blocks, participantBlock{box: svc, names: names})
}

// declare emits a participant line for every planned lifeline that ref marks as
// touched by an edge, preserving plan order and boxing. A box collapses to a loose
// participant when only its service (no owned database) was referenced.
func (p *participantPlan) declare(b *strings.Builder, ref map[string]bool) {
	declared := map[string]bool{}
	one := func(name string) {
		if !ref[name] || declared[name] {
			return
		}
		declared[name] = true
		b.WriteString("    participant " + p.r.aliasOf(name) + " as " + name + "\n")
	}
	for _, bl := range p.blocks {
		if bl.box == "" {
			for _, n := range bl.names {
				one(n)
			}
			continue
		}
		var members []string
		for _, n := range bl.names {
			if ref[n] && !declared[n] {
				members = append(members, n)
			}
		}
		if len(members) <= 1 { // nothing, or only the bare service: no box
			for _, n := range members {
				one(n)
			}
			continue
		}
		// Mermaid parses `box [color] [title]`, so a service named a color word (e.g.
		// "aqua") would be swallowed as the box color. Emit an explicit `transparent`
		// color so the service name always lands in the title slot.
		b.WriteString("    box transparent " + bl.box + "\n")
		for _, n := range members {
			one(n)
		}
		b.WriteString("    end\n")
	}
}

// landingOf is the lifeline an operation lands on: for an inbound entry
// (server/consumer) or internal op, its own owning service; for an outbound call
// (client/producer), the counterparty Peer. The fallback (the trace's Service)
// covers a span with no folded service.name.
func landingOf(s *ir.CanonicalSpan, fallback string) string {
	switch s.Kind {
	case ir.KindClient, ir.KindProducer:
		if s.Peer != "" {
			return s.Peer
		}
		return lifelineLabel(s.Service, fallback)
	default: // server, consumer, internal
		return lifelineLabel(s.Service, fallback)
	}
}

// serviceInfra classifies the flow's lifelines for the boxed participant layout.
// It returns the set of services (lifelines that own spans — an inbound entry,
// internal work, or a span carrying service.name) and, for each service, the
// database lifelines it exclusively owns: a database is owned when every DB hop to
// it originates from one service. A database touched by more than one service, a
// database that is itself a service, and every non-database peer (the broker,
// external services) are shared and left unboxed.
func serviceInfra(root *ir.CanonicalSpan, fallback string) (services map[string]bool, ownedDB map[string][]string) {
	services = map[string]bool{}
	dbOwners := map[string]map[string]bool{} // db lifeline -> owning services
	// from is the lifeline an inbound hop into s was drawn from — the same threaded
	// parent landing the renderer uses (writeSystemSpan), so ownership agrees with the
	// arrow actually drawn.
	var walk func(s *ir.CanonicalSpan, from string)
	walk = func(s *ir.CanonicalSpan, from string) {
		if s == nil {
			return
		}
		if s.Service != "" {
			services[s.Service] = true
		}
		switch s.Kind {
		case ir.KindServer, ir.KindConsumer, ir.KindInternal:
			services[landingOf(s, fallback)] = true
		}
		// A database lifeline exists only where the DB hop actually lands on the peer
		// (an outbound client DB call). Attribute it to the lifeline the hop is drawn
		// FROM — not the span's own service.name — so the owning box and the drawn arrow
		// always agree even when a DB span is nested under a same-service client call
		// (where the threaded from is the call's peer, not the db's service). An
		// ORM-emitted internal DB op lands on its own service (landingOf != Peer) and is
		// never drawn, so it seeds no DB participant — mirroring the drawTarget rule.
		if s.Peer != "" && landingOf(s, fallback) == s.Peer && strings.HasPrefix(s.Op, opkey.DBPrefix) {
			if dbOwners[s.Peer] == nil {
				dbOwners[s.Peer] = map[string]bool{}
			}
			dbOwners[s.Peer][from] = true
		}
		childFrom := landingOf(s, fallback)
		for _, g := range s.Children {
			for _, m := range g.Members {
				walk(m, childFrom)
			}
		}
	}
	walk(root, landingOf(root, fallback))

	ownedDB = map[string][]string{}
	for db, owners := range dbOwners {
		if services[db] || len(owners) != 1 {
			continue // shared across services, or itself a service: leave unboxed
		}
		for o := range owners {
			ownedDB[o] = append(ownedDB[o], db)
		}
	}
	for s := range ownedDB {
		sort.Strings(ownedDB[s])
	}
	return services, ownedDB
}

func (r *renderer) writeSystemGroups(b *strings.Builder, groups []ir.ChildGroup, from, fallback, indent string) {
	for _, g := range groups {
		// Synchronous members keep the group's ordering framing. Async (FOLLOWS_FROM)
		// continuations — consumers separately polled later — must not nest in the
		// producer's synchronous block, where they read as calls made during the
		// publish; they render as distinct dashed interactions, but still keep the
		// group's framing among themselves when several consumers fan out at once.
		sync, async := splitAsync(g.Members)
		r.writeMembers(b, g, sync, indent, func(m *ir.CanonicalSpan, ind string) {
			r.writeSystemSpan(b, m, from, fallback, ind)
		})
		r.writeMembers(b, g, async, indent, func(m *ir.CanonicalSpan, ind string) {
			r.writeAsyncSystemSpan(b, m, from, fallback, ind)
		})
		if g.Multiplicity != "" {
			b.WriteString(indent + "Note over " + r.id(from) + ": ×" + g.Multiplicity + "\n")
		}
	}
}

// writeMembers renders members with the group's ordering framing: a concurrent or
// unordered group of two or more members becomes a par/and/end block; a lone member
// (a deduped loop representative or a sequential step) renders inline. draw emits one
// member's hop and subtree, so the same framing serves both synchronous hops and
// dashed async continuations. Empty members render nothing.
func (r *renderer) writeMembers(b *strings.Builder, g ir.ChildGroup, members []*ir.CanonicalSpan, indent string, draw func(m *ir.CanonicalSpan, indent string)) {
	if len(members) == 0 {
		return
	}
	if (g.Concurrent || g.Unordered) && len(members) > 1 {
		b.WriteString(indent + "par " + groupLabel(g) + "\n")
		for i, m := range members {
			if i > 0 {
				b.WriteString(indent + "and\n")
			}
			draw(m, indent+"    ")
		}
		b.WriteString(indent + "end\n")
		return
	}
	for _, m := range members {
		draw(m, indent)
	}
}

// writeAsyncSystemSpan renders a FOLLOWS_FROM continuation — a consumer polled
// later, caused by (not called during) the producer's work — as a distinct async
// interaction: a dashed, open-arrow hop and a Note marking it asynchronous, drawn
// outside any synchronous block. Its subtree then renders normally. As in
// writeSystemSpan, a span that lands on the lifeline it was already reached from
// draws no redundant arrow (and so no async note); for a real link-stitched consumer
// that never coincides, since it lands on its own service.
func (r *renderer) writeAsyncSystemSpan(b *strings.Builder, m *ir.CanonicalSpan, from, fallback, indent string) {
	if drawTo := drawTarget(m, from, fallback); drawTo != from {
		b.WriteString(indent + r.amsg(from, drawTo, label(m)))
		b.WriteString(indent + "Note over " + r.id(drawTo) + ": async (FOLLOWS_FROM)\n")
	}
	r.writeSystemGroups(b, m.Children, landingOf(m, fallback), fallback, indent)
}

// splitAsync partitions a group's members into synchronous members and async
// (link-caused) continuations, preserving order within each partition.
func splitAsync(members []*ir.CanonicalSpan) (sync, async []*ir.CanonicalSpan) {
	for _, m := range members {
		if m.Async {
			async = append(async, m)
		} else {
			sync = append(sync, m)
		}
	}
	return sync, async
}

// drawTarget is the lifeline a hop into m is drawn to: normally where m lands, but a
// self-landing consumer poll (an SQS ReceiveMessage lands on its own service rather
// than the bus) draws to its broker peer so the receive stays as visible as the
// publish and the settle. Restricted to KindConsumer so a peer-bearing self-landing
// span of another kind (an ORM-emitted internal DB op, whose peer is the db system)
// is not drawn — matching serviceInfra, which counts a DB participant only where the
// hop lands on the peer. Shared by the synchronous and async hop renderers so the
// rule has one definition.
func drawTarget(m *ir.CanonicalSpan, from, fallback string) string {
	land := landingOf(m, fallback)
	if land == from && m.Kind == ir.KindConsumer && m.Peer != "" && m.Peer != from {
		return m.Peer
	}
	return land
}

// writeSystemSpan draws the hop into a span (from the caller lifeline to where the
// span lands) and recurses, threading the landed lifeline as the new caller. When
// the span lands on the lifeline it was already called from — a callee's own
// entry span, or an internal self-op — no redundant arrow is drawn; the call that
// arrived there is enough.
func (r *renderer) writeSystemSpan(b *strings.Builder, m *ir.CanonicalSpan, from, fallback, indent string) {
	if drawTo := drawTarget(m, from, fallback); drawTo != from {
		b.WriteString(indent + r.msg(from, drawTo, label(m)))
	}
	r.writeSystemGroups(b, m.Children, landingOf(m, fallback), fallback, indent)
}

// writeParticipants declares lifelines in a fixed order: the caller, the self
// service, then every peer sorted. Each is aliased to a Mermaid-safe id so names
// with hyphens (credit-bureau) render.
func (r *renderer) writeParticipants(b *strings.Builder, t *ir.CanonicalTrace) {
	peers := map[string]bool{}
	collectPeers(t.Root, peers)

	order := []string{r.caller, r.self}
	rest := make([]string, 0, len(peers))
	for p := range peers {
		if p != r.caller && p != r.self {
			rest = append(rest, p)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	for _, name := range order {
		if _, ok := r.alias[name]; ok {
			continue
		}
		b.WriteString("    participant " + r.aliasOf(name) + " as " + name + "\n")
	}
}

// writeGroups renders ordered child groups: sequential groups inline, concurrent
// groups as par/and/end, and a collapsed loop as a multiplicity note.
func (r *renderer) writeGroups(b *strings.Builder, groups []ir.ChildGroup, indent string) {
	for _, g := range groups {
		if g.Concurrent || g.Unordered {
			b.WriteString(indent + "par " + groupLabel(g) + "\n")
			for i, m := range g.Members {
				if i > 0 {
					b.WriteString(indent + "and\n")
				}
				r.writeSpan(b, m, indent+"    ")
			}
			b.WriteString(indent + "end\n")
		} else {
			for _, m := range g.Members {
				r.writeSpan(b, m, indent)
			}
		}
		if g.Multiplicity != "" {
			b.WriteString(indent + "Note over " + r.id(r.self) + ": ×" + g.Multiplicity + "\n")
		}
	}
}

// writeSpan renders one operation as a message from self to its lifeline, then
// recurses into any retained sub-operations (still issued by self).
func (r *renderer) writeSpan(b *strings.Builder, m *ir.CanonicalSpan, indent string) {
	target := r.self
	if m.Peer != "" {
		target = m.Peer
	}
	b.WriteString(indent + r.msg(r.self, target, label(m)))
	r.writeGroups(b, m.Children, indent)
}

// msg formats one synchronous arrow line, resolving lifeline labels to aliases.
func (r *renderer) msg(from, to, text string) string {
	return r.id(from) + "->>" + r.id(to) + ": " + text + "\n"
}

// amsg formats one asynchronous arrow line — a dashed, open arrowhead (Mermaid's
// `--)`) — distinguishing a link-caused continuation from a synchronous call.
func (r *renderer) amsg(from, to, text string) string {
	return r.id(from) + "--)" + r.id(to) + ": " + text + "\n"
}

func (r *renderer) id(label string) string {
	r.ref[label] = true // this lifeline is touched by the arrow/note being drawn
	return r.aliasOf(label)
}

// groupLabel distinguishes a genuine race (concurrent) from siblings whose order
// could not be established (unordered) in a par block's label.
func groupLabel(g ir.ChildGroup) string {
	if g.Unordered {
		return "unordered"
	}
	return "concurrent"
}

// label is the message text for a span: its canonical op, annotated with the
// error class when the operation failed.
func label(s *ir.CanonicalSpan) string {
	if s.Status == "error" {
		et := s.ErrorType
		if et == "" {
			et = "error"
		}
		return s.Op + " [" + et + "]"
	}
	return s.Op
}

// callerLabel is the lifeline that triggered the flow: a generic Client for an
// inbound HTTP server root, and for a consumed-event root the broker it consumed
// from. That broker is the root's own Peer, which opkey.brokerPeer already
// canonicalizes per messaging system (Bus / SNS / SQS / …), so the trigger lifeline
// coincides with the consumer root's own peer — rather than a hardcoded "Bus" that
// would draw the trigger arrow from a lifeline distinct from the real broker. "Bus"
// remains the fallback for a hand-built consumer root carrying no peer.
func callerLabel(root *ir.CanonicalSpan) string {
	if root != nil && root.Kind == ir.KindConsumer {
		if root.Peer != "" {
			return root.Peer
		}
		return "Bus"
	}
	return "Client"
}

func collectPeers(s *ir.CanonicalSpan, into map[string]bool) {
	if s == nil {
		return
	}
	if s.Peer != "" {
		into[s.Peer] = true
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			collectPeers(m, into)
		}
	}
}

func lifelineLabel(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// sanitize converts a lifeline label into a Mermaid-safe identifier: leading
// alpha, then alphanumerics, with everything else collapsed to underscores.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	id := b.String()
	if id == "" || !isASCIILetter(id[0]) {
		id = "L" + id
	}
	return id
}

// isASCIILetter reports whether c is an ASCII letter (a valid leading character
// for a Mermaid identifier).
func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func uniqueID(base string, used map[string]bool) string {
	id := base
	for i := 1; used[id]; i++ {
		id = base + strconv.Itoa(i)
	}
	used[id] = true
	return id
}
