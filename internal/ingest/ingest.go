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
	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
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
// output is stable. Span links are stitched first (stitch), so a consumer that
// joins a flow across a broker hand-off — arriving on its own trace without the
// flow baggage — is recovered and gated as its own per-service fragment.
func Group(spans []capture.Span) []FlowCapture {
	spans = stitch(spans)
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
	spans, root, trigger, synth := assembleRoot(svc, spans)
	return FlowCapture{
		Slug:        slug,
		Service:     svc,
		Synthesized: synth,
		Flow: capture.CapturedFlow{
			Flow:     slug,
			Service:  svc,
			Trigger:  trigger,
			Mode:     capture.ModePostHoc,
			Spans:    spans,
			Root:     root,
			Complete: true,
		},
	}
}

// WholeFlows is the cross-service analog of Group, for rendering rather than
// gating (design D-PH1: the diagram unit). It buckets spans by flow slug only —
// no per-service split — and assembles one tree spanning every service the flow
// touched, joined by parent_span_id (causal links survive in OTLP, so no
// cross-clock comparison is needed) and by span links across broker hand-offs
// (stitch reparents a link-joined new-root span onto its producer, so an async
// continuation in a separate trace lands in the same tree). The result feeds
// canonicalization and the cross-service renderer; it is never gated. The
// trace's Service is the entry service (the root span's owning service).
func WholeFlows(spans []capture.Span) []FlowCapture {
	spans = stitch(spans)
	buckets := map[string][]capture.Span{}
	var order []string
	for _, s := range spans {
		slug := s.Attr(FlowKey)
		if slug == "" {
			continue
		}
		if _, ok := buckets[slug]; !ok {
			order = append(order, slug)
		}
		buckets[slug] = append(buckets[slug], s)
	}
	sort.Strings(order)

	out := make([]FlowCapture, 0, len(order))
	for _, slug := range order {
		spans, root, trigger, synth := assembleRoot(slug, buckets[slug])
		// The entry service owns the trace lifeline. A synthesized root (no single
		// inbound entry — a multi-trace slug or an event-only flow) carries no
		// service.name. When every span in the flow belongs to one service, that
		// service is the unambiguous owner and names the lifeline (a test that
		// awaits several requests against one service synthesizes a root, but the
		// flow is still that service's). Only when the flow spans multiple services
		// — so no single owner exists — does it fall back to the flow slug, rather
		// than an empty lifeline the renderer would draw as an unnamed participant.
		// This is a render-only label: the synthesized span's own Service is left
		// empty, so the per-span attribution the system-context graph keys on (and
		// its ingress-edge recovery) is unchanged.
		svc := root.Attr(serviceKey)
		if svc == "" {
			svc = commonService(spans)
		}
		if svc == "" {
			svc = slug
		}
		out = append(out, FlowCapture{
			Slug:        slug,
			Service:     svc,
			Synthesized: synth,
			Flow: capture.CapturedFlow{
				Flow:     slug,
				Service:  svc,
				Trigger:  trigger,
				Mode:     capture.ModePostHoc,
				Spans:    spans,
				Root:     root,
				Complete: true,
			},
		})
	}
	return out
}

// assembleRoot finds (or synthesizes) the entry root for a span set. A single
// parentless server/consumer span is the natural entry; otherwise it synthesizes
// an internal root owning every parentless span, so canonicalization sees one
// tree. synthName names the synthetic root. It returns the possibly-extended span
// set (the synthetic root appended), the root pointer into that set, the trigger
// kind, and whether a root was synthesized.
func assembleRoot(synthName string, spans []capture.Span) ([]capture.Span, *capture.Span, capture.TriggerKind, bool) {
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

	trigger := capture.TriggerHTTP
	if len(parentless) == 1 {
		s := &spans[parentless[0]]
		// Classify the entry by its EFFECTIVE kind: an AWS-SDK consumer roots its
		// own trace as a CLIENT span carrying messaging consume attributes, so the
		// raw kind would miss it and force a spurious synthetic root. EffectiveKind
		// maps it to KindConsumer (and leaves a true server/consumer unchanged).
		switch opkey.EffectiveKind(s.Kind, s.Attrs) {
		case ir.KindServer:
			return spans, s, capture.TriggerHTTP, false
		case ir.KindConsumer:
			return spans, s, capture.TriggerEvent, false
		}
	}
	syn := capture.Span{
		ID:    "flowmap-root:" + synthName,
		Name:  synthName,
		Kind:  ir.KindInternal,
		Attrs: map[string]string{},
	}
	for _, i := range parentless {
		spans[i].ParentID = syn.ID
	}
	spans = append(spans, syn)
	return spans, &spans[len(spans)-1], trigger, true
}

// commonService returns the single service.name shared by every span carrying
// one, or "" if the spans span more than one service (no single owner) or none
// name a service. The synthesized root's empty service is ignored.
func commonService(spans []capture.Span) string {
	svc := ""
	for i := range spans {
		s := spans[i].Attr(serviceKey)
		if s == "" {
			continue
		}
		if svc == "" {
			svc = s
		} else if svc != s {
			return ""
		}
	}
	return svc
}

// reachesUp reports whether target is start or one of its ancestors, following
// ParentID links via byID. maxHops bounds the walk so a pre-existing cycle in the
// input cannot spin forever. Used to reject a reparent that would close a cycle.
func reachesUp(byID map[string]*capture.Span, start, target string, maxHops int) bool {
	cur := start
	for n := 0; n <= maxHops && cur != ""; n++ {
		if cur == target {
			return true
		}
		s, ok := byID[cur]
		if !ok {
			return false
		}
		cur = s.ParentID
	}
	return false
}

// skey is a span's global identity. A flow that crosses a broker spans multiple
// traces, and OTLP span ids are unique only within a trace, so identity is the
// (traceId, spanId) pair. In-process fixtures and pre-stitch tests carry no
// TraceID; those keep their bare span id, leaving existing single-trace
// grouping untouched.
func skey(traceID, spanID string) string {
	if traceID == "" {
		return spanID
	}
	return traceID + "|" + spanID
}

// stitch joins the traces of an async flow into one connected span set before
// grouping, using OTLP span links as the cross-trace membership signal. The
// flow baggage that carries the slug does not cross a broker (correctly — the
// consumer runs later, on its own trace), so a consumer's spans arrive without
// FlowKey and parent_span_id does not reach back to the producer. The consumer's
// entry span instead carries a link (FOLLOWS_FROM) to the producer span it
// processed; stitch follows that link to (a) reparent the consumer subtree onto
// the producer and (b) propagate the producer's flow slug across the hand-off.
//
// It rewrites span ids to global (traceId, spanId) keys so parent and link
// references resolve across traces without collision, follows links only on
// genuine new roots (original parent_span_id empty) so a mid-trace causal link
// never rewires the tree, and propagates the slug down parent edges to a
// fixpoint so a multi-hop async chain (produce → consume → produce → consume)
// is fully recovered. Inputs are copied; the caller's spans are not mutated.
func stitch(spans []capture.Span) []capture.Span {
	if len(spans) == 0 {
		return spans
	}
	out := make([]capture.Span, len(spans))
	copy(out, spans)

	byID := make(map[string]*capture.Span, len(out))
	wasRoot := make([]bool, len(out))
	for i := range out {
		wasRoot[i] = out[i].ParentID == ""
		out[i].ID = skey(out[i].TraceID, out[i].ID)
		if out[i].ParentID != "" {
			out[i].ParentID = skey(out[i].TraceID, out[i].ParentID)
		}
		byID[out[i].ID] = &out[i]
	}

	// Cross-trace tree stitch: a new-root span (a consumer beginning a fresh
	// trace) whose link targets a known span is reparented onto that target, so
	// the async continuation joins the producer's tree. The same edge carries
	// slug membership below.
	//
	// A reparent is skipped when the target is the span itself or an own descendant
	// — that would form a parent cycle and drop the span (it becomes unreachable
	// from any root, so canon's assembly silently discards its whole subtree).
	// Because each edge is validated against the parent state built so far, no
	// cycle can ever form (two spans linking to each other keep the first edge and
	// drop the second). Among valid links the one whose target already carries the
	// flow tag — the producer that propagates membership — is preferred over an
	// incidental link (e.g. a batch consumer linking several messages); otherwise
	// the first valid link wins.
	for i := range out {
		if !wasRoot[i] {
			continue
		}
		fallback := ""
		for _, l := range out[i].Links {
			tk := skey(l.TraceID, l.SpanID)
			t, ok := byID[tk]
			if !ok || reachesUp(byID, tk, out[i].ID, len(out)) {
				continue
			}
			if t.Attr(FlowKey) != "" {
				fallback = tk
				break
			}
			if fallback == "" {
				fallback = tk
			}
		}
		if fallback != "" {
			out[i].ParentID = fallback
			// Mark the edge as a cross-broker async continuation so the renderer can
			// draw the hop into this span (a separately-polled consumer) distinctly,
			// not as a synchronous call nested in the producer's block.
			out[i].AsyncLink = true
		}
	}

	// Propagate flow-slug membership down parent edges (which now include the
	// stitched link edges) to a fixpoint, so every span reachable from a
	// flow-tagged entry inherits the slug even though its baggage was lost at the
	// broker.
	slug := make(map[string]string, len(out))
	for i := range out {
		if s := out[i].Attr(FlowKey); s != "" {
			slug[out[i].ID] = s
		}
	}
	for changed := true; changed; {
		changed = false
		for i := range out {
			if slug[out[i].ID] != "" {
				continue
			}
			if s := slug[out[i].ParentID]; s != "" {
				slug[out[i].ID] = s
				changed = true
			}
		}
	}

	// Write recovered slugs back so Group/WholeFlows bucket the joined spans.
	// Attrs maps are shared with the caller's input, so copy-on-write before
	// adding the key.
	for i := range out {
		s := slug[out[i].ID]
		if s == "" || out[i].Attr(FlowKey) != "" {
			continue
		}
		m := make(map[string]string, len(out[i].Attrs)+1)
		for k, v := range out[i].Attrs {
			m[k] = v
		}
		m[FlowKey] = s
		out[i].Attrs = m
	}
	return out
}
