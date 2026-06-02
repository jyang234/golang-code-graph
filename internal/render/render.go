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

	"github.com/jyang234/golang-code-graph/ir"
)

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

	// Lifelines in a fixed order: caller, entry service, then every other
	// service/peer the flow reaches, sorted.
	lifelines := map[string]bool{}
	collectSystemLifelines(root, fallback, lifelines)
	order := []string{caller, entry}
	rest := make([]string, 0, len(lifelines))
	for l := range lifelines {
		if l != caller && l != entry {
			rest = append(rest, l)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	used := map[string]bool{}
	for _, name := range order {
		if _, ok := r.alias[name]; ok {
			continue
		}
		id := uniqueID(sanitize(name), used)
		r.alias[name] = id
		b.WriteString("    participant " + id + " as " + name + "\n")
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

func collectSystemLifelines(s *ir.CanonicalSpan, fallback string, into map[string]bool) {
	if s == nil {
		return
	}
	into[landingOf(s, fallback)] = true
	if s.Service != "" {
		into[s.Service] = true
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			collectSystemLifelines(m, fallback, into)
		}
	}
}

func (r *renderer) writeSystemGroups(b *strings.Builder, groups []ir.ChildGroup, from, fallback, indent string) {
	for _, g := range groups {
		if g.Concurrent {
			b.WriteString(indent + "par concurrent\n")
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

// writeSystemSpan draws the hop into a span (from the caller lifeline to where the
// span lands) and recurses, threading the landed lifeline as the new caller. When
// the span lands on the lifeline it was already called from — a callee's own
// entry span, or an internal self-op — no redundant arrow is drawn; the call that
// arrived there is enough.
func (r *renderer) writeSystemSpan(b *strings.Builder, m *ir.CanonicalSpan, from, fallback, indent string) {
	land := landingOf(m, fallback)
	if land != from {
		b.WriteString(indent + r.msg(from, land, label(m)))
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
		if g.Concurrent {
			b.WriteString(indent + "par concurrent\n")
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

// msg formats one arrow line, resolving lifeline labels to their aliases.
func (r *renderer) msg(from, to, text string) string {
	return r.id(from) + "->>" + r.id(to) + ": " + text + "\n"
}

func (r *renderer) id(label string) string {
	if id, ok := r.alias[label]; ok {
		return id
	}
	return sanitize(label)
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
