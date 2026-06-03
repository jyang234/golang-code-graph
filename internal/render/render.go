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
	r := &renderer{self: lifelineLabel(t.Service, "service")}
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
	var b strings.Builder
	b.WriteString("sequenceDiagram\n")
	r := &renderer{alias: map[string]string{}}
	entry := landingOf(root, fallback)

	lifelines := map[string]bool{}
	collectSystemLifelines(root, fallback, lifelines)
	lifelines[caller] = true
	lifelines[entry] = true

	// Participants are laid out family-adjacent: the synthetic caller, then each
	// service immediately followed by — and boxed with — the databases it
	// exclusively owns (a store reached only from that service), then the remaining
	// shared lifelines (the broker, external peers, multi-owner databases) sorted.
	// This keeps a service and its infrastructure together instead of interleaving
	// every service's databases alphabetically.
	services, ownedDB := serviceInfra(root, fallback)
	used := map[string]bool{}
	decl := func(name string) {
		if name == "" {
			return
		}
		if _, ok := r.alias[name]; ok {
			return
		}
		id := uniqueID(sanitize(name), used)
		r.alias[name] = id
		b.WriteString("    participant " + id + " as " + name + "\n")
	}

	decl(caller) // the ingress/bus, never boxed with a service

	svcOrder := make([]string, 0, len(services)+1)
	if entry != caller {
		svcOrder = append(svcOrder, entry)
	}
	rest := make([]string, 0, len(services))
	for s := range services {
		if s != entry && s != caller {
			rest = append(rest, s)
		}
	}
	sort.Strings(rest)
	svcOrder = append(svcOrder, rest...)

	for _, svc := range svcOrder {
		if svc == "" {
			continue
		}
		if infra := ownedDB[svc]; len(infra) > 0 {
			b.WriteString("    box " + svc + "\n")
			decl(svc)
			for _, db := range infra {
				decl(db)
			}
			b.WriteString("    end\n")
		} else {
			decl(svc)
		}
	}

	// Whatever is left (broker, external peers, shared/multi-owner databases).
	remaining := make([]string, 0, len(lifelines))
	for l := range lifelines {
		if _, ok := r.alias[l]; !ok {
			remaining = append(remaining, l)
		}
	}
	sort.Strings(remaining)
	for _, l := range remaining {
		decl(l)
	}

	b.WriteString("    " + r.msg(caller, entry, label(root)))
	r.writeSystemGroups(&b, root.Children, entry, fallback, "    ")
	return b.String()
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
	var walk func(s *ir.CanonicalSpan)
	walk = func(s *ir.CanonicalSpan) {
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
		// (an outbound client DB call). An ORM-emitted internal DB op lands on its own
		// service and is never drawn, so it must not seed a phantom DB participant —
		// mirroring collectSystemLifelines / writeSystemSpan.
		if s.Peer != "" && landingOf(s, fallback) == s.Peer && strings.HasPrefix(s.Op, opkey.DBPrefix) {
			if dbOwners[s.Peer] == nil {
				dbOwners[s.Peer] = map[string]bool{}
			}
			dbOwners[s.Peer][lifelineLabel(s.Service, fallback)] = true
		}
		for _, g := range s.Children {
			for _, m := range g.Members {
				walk(m)
			}
		}
	}
	walk(root)

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

func collectSystemLifelines(s *ir.CanonicalSpan, fallback string, into map[string]bool) {
	if s == nil {
		return
	}
	into[landingOf(s, fallback)] = true
	if s.Service != "" {
		into[s.Service] = true
	}
	// A consumer poll lands on its own service but draws its hop to the bus
	// (writeSystemSpan); declare that peer so it has a fixed-order participant.
	if s.Kind == ir.KindConsumer && s.Peer != "" {
		into[s.Peer] = true
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			collectSystemLifelines(m, fallback, into)
		}
	}
}

func (r *renderer) writeSystemGroups(b *strings.Builder, groups []ir.ChildGroup, from, fallback, indent string) {
	for _, g := range groups {
		sync, async := splitAsync(g.Members)
		if len(async) > 0 {
			// A link-caused continuation (a consumer separately polled later) must not
			// be nested in the producer's synchronous block, where it reads as a call
			// made during the publish. Render the synchronous members with the group's
			// own semantics, then each async member as its own distinct interaction.
			r.writeSyncMembers(b, g, sync, from, fallback, indent)
			for _, m := range async {
				r.writeAsyncSystemSpan(b, m, from, fallback, indent)
			}
		} else if g.Concurrent || g.Unordered {
			b.WriteString(indent + "par " + groupLabel(g) + "\n")
			for i, m := range g.Members {
				if i > 0 {
					b.WriteString(indent + "and\n")
				}
				r.writeSystemSpan(b, m, from, fallback, indent+"    ")
			}
			b.WriteString(indent + "end\n")
		} else {
			for _, m := range g.Members {
				r.writeSystemSpan(b, m, from, fallback, indent)
			}
		}
		if g.Multiplicity != "" {
			b.WriteString(indent + "Note over " + r.id(from) + ": ×" + g.Multiplicity + "\n")
		}
	}
}

// writeSyncMembers renders the synchronous members extracted from a group with the
// group's ordering semantics: a concurrent/unordered group of two or more becomes a
// par/and/end block; a lone member (or a sequential group) renders inline. Empty
// renders nothing — a group of purely async continuations draws no sync block.
func (r *renderer) writeSyncMembers(b *strings.Builder, g ir.ChildGroup, members []*ir.CanonicalSpan, from, fallback, indent string) {
	if len(members) == 0 {
		return
	}
	if (g.Concurrent || g.Unordered) && len(members) > 1 {
		b.WriteString(indent + "par " + groupLabel(g) + "\n")
		for i, m := range members {
			if i > 0 {
				b.WriteString(indent + "and\n")
			}
			r.writeSystemSpan(b, m, from, fallback, indent+"    ")
		}
		b.WriteString(indent + "end\n")
		return
	}
	for _, m := range members {
		r.writeSystemSpan(b, m, from, fallback, indent)
	}
}

// writeAsyncSystemSpan renders a FOLLOWS_FROM continuation — a consumer polled
// later, caused by (not called during) the producer's work — as a distinct async
// interaction: a dashed, open-arrow hop and a Note marking it asynchronous, drawn
// outside any synchronous block. Its subtree then renders normally.
func (r *renderer) writeAsyncSystemSpan(b *strings.Builder, m *ir.CanonicalSpan, from, fallback, indent string) {
	land := landingOf(m, fallback)
	drawTo := land
	if land == from && m.Kind == ir.KindConsumer && m.Peer != "" && m.Peer != from {
		drawTo = m.Peer
	}
	if drawTo != from {
		b.WriteString(indent + r.amsg(from, drawTo, label(m)))
		b.WriteString(indent + "Note over " + r.id(drawTo) + ": async (FOLLOWS_FROM)\n")
	}
	r.writeSystemGroups(b, m.Children, land, fallback, indent)
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

// writeSystemSpan draws the hop into a span (from the caller lifeline to where the
// span lands) and recurses, threading the landed lifeline as the new caller. When
// the span lands on the lifeline it was already called from — a callee's own
// entry span, or an internal self-op — no redundant arrow is drawn; the call that
// arrived there is enough.
func (r *renderer) writeSystemSpan(b *strings.Builder, m *ir.CanonicalSpan, from, fallback, indent string) {
	land := landingOf(m, fallback)
	drawTo := land
	if land == from && m.Kind == ir.KindConsumer && m.Peer != "" && m.Peer != from {
		// A consumer poll / receive (SQS ReceiveMessage) is KindConsumer and so
		// lands on its own service rather than the bus; nested under that same
		// service it would otherwise draw no hop and vanish. Draw the reach to its
		// peer (the Bus) so the receive is as visible as the publish and the settle.
		// Restricted to KindConsumer so a peer-bearing self-landing span of another
		// kind (an ORM-emitted internal DB op, whose peer is the db system) is not
		// drawn here — that matches collectSystemLifelines, which declares the peer
		// participant only for KindConsumer. Child threading stays on the landed
		// lifeline.
		drawTo = m.Peer
	}
	if drawTo != from {
		b.WriteString(indent + r.msg(from, drawTo, label(m)))
	}
	r.writeSystemGroups(b, m.Children, land, fallback, indent)
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

	r.alias = make(map[string]string, len(order))
	used := map[string]bool{}
	for _, name := range order {
		if _, ok := r.alias[name]; ok {
			continue
		}
		id := uniqueID(sanitize(name), used)
		r.alias[name] = id
		b.WriteString("    participant " + id + " as " + name + "\n")
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
	if id, ok := r.alias[label]; ok {
		return id
	}
	return sanitize(label)
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
// inbound HTTP server root, the Bus for a consumed event.
func callerLabel(root *ir.CanonicalSpan) string {
	if root != nil && root.Kind == ir.KindConsumer {
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
