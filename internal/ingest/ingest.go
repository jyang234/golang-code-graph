// Package ingest groups a flat set of post-hoc spans (decoded from an OTLP/JSON
// trace export) into per-flow, per-service CapturedFlows ready for the existing
// canonicalizer — the out-of-process analog of the in-process harness's
// scope-and-assemble step (capture.Scope, post-hoc design [P10.2]).
//
// Two reductions of the same spans serve two consumers (design D-PH1): the
// assertion/coverage unit is the flow slug (a withFlow block can issue several
// requests, each rooting its own trace, so a slug spans multiple traces), and a
// representative trace is the diagram unit. This package produces the per-slug,
// per-service fragments; the caller canonicalizes and unions them.
//
// Per design D-PH4 a cross-service trace is split by service.name, so each
// service is validated against its own spans and owns its own golden. A
// fragment's entry is the span whose parent lives outside the fragment (an
// inbound server/consumer span, or — for a publisher-only service with no
// inbound span — a synthesized internal root, flagged so the caller can warn).
package ingest

import (
	"sort"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// FlowKey is the span attribute (promoted from baggage by a per-service
// baggagecopy processor, see docs/integration) that tags a span as belonging to
// a named flow and supplies the golden's slug.
const FlowKey = "flowmap.flow"

// serviceKey is the OTel resource attribute naming the emitting service; it is
// the per-service split key (design D-PH4).
const serviceKey = "service.name"

// FlowCapture is one ingested fragment: the spans for a single flow slug emitted
// by a single service, assembled into a CapturedFlow the canonicalizer accepts.
type FlowCapture struct {
	Slug        string
	Service     string
	Flow        capture.CapturedFlow
	Synthesized bool // the root was synthesized (no single inbound entry span)
}

// Group partitions spans by (flow slug, service) and assembles each partition
// into a CapturedFlow. Spans not carrying FlowKey are ignored — the export may
// contain unrelated traffic. The result is ordered by slug then service so the
// output is stable.
func Group(spans []capture.Span) []FlowCapture {
	type key struct{ slug, svc string }
	buckets := map[key][]capture.Span{}
	var order []key
	for _, s := range spans {
		slug := s.Attr(FlowKey)
		if slug == "" {
			continue
		}
		k := key{slug, s.Attr(serviceKey)}
		if _, ok := buckets[k]; !ok {
			order = append(order, k)
		}
		buckets[k] = append(buckets[k], s)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].slug != order[j].slug {
			return order[i].slug < order[j].slug
		}
		return order[i].svc < order[j].svc
	})

	out := make([]FlowCapture, 0, len(order))
	for _, k := range order {
		out = append(out, assemble(k.slug, k.svc, buckets[k]))
	}
	return out
}

// assemble reconstructs one fragment's root. A single parentless server/consumer
// span is the natural entry. Otherwise (zero, several, or a non-entry parentless
// span — a publisher-only service, or a fragment whose entry's parent is a
// remote client span in another service) it synthesizes an internal root owning
// every parentless span, so canonicalization sees one tree rather than refusing
// the capture. Completeness is trusted here (Complete=true): stage 1 is
// observational and never gates, and the caller surfaces a Synthesized fragment
// as a warning (design D-PH2).
func assemble(slug, svc string, spans []capture.Span) FlowCapture {
	ids := make(map[string]bool, len(spans))
	for i := range spans {
		ids[spans[i].ID] = true
	}
	var parentless []int
	for i := range spans {
		if !ids[spans[i].ParentID] {
			parentless = append(parentless, i)
		}
	}

	fc := FlowCapture{Slug: slug, Service: svc}
	var root *capture.Span
	trigger := capture.TriggerHTTP
	if len(parentless) == 1 {
		s := &spans[parentless[0]]
		switch s.Kind {
		case ir.KindServer:
			root, trigger = s, capture.TriggerHTTP
		case ir.KindConsumer:
			root, trigger = s, capture.TriggerEvent
		}
	}
	if root == nil {
		syn := capture.Span{
			ID:    "flowmap-root:" + slug + ":" + svc,
			Name:  svc,
			Kind:  ir.KindInternal,
			Attrs: map[string]string{},
		}
		for _, i := range parentless {
			spans[i].ParentID = syn.ID
		}
		spans = append(spans, syn)
		root = &spans[len(spans)-1]
		fc.Synthesized = true
	}

	fc.Flow = capture.CapturedFlow{
		Flow:     slug,
		Service:  svc,
		Trigger:  trigger,
		Mode:     capture.ModePostHoc,
		Spans:    spans,
		Root:     root,
		Complete: true,
	}
	return fc
}
