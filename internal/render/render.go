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
	if id == "" || !(id[0] >= 'a' && id[0] <= 'z' || id[0] >= 'A' && id[0] <= 'Z') {
		id = "L" + id
	}
	return id
}

func uniqueID(base string, used map[string]bool) string {
	id := base
	for i := 1; used[id]; i++ {
		id = base + itoa(i)
	}
	used[id] = true
	return id
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
