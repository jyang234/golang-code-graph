// Package capture is flowmap's PUBLIC raw-trace model and the harness's output:
// a complete, scoped trace for exactly one flow (trace-capture-harness spec §7).
// It is the stable data contract handed from the harness to canonicalization —
// the raw counterpart of the canonical ir.CanonicalTrace — so an advanced
// consumer can drive a capture and inspect the result directly, not only through
// the flow DSL. It is deliberately free of any OpenTelemetry dependency: the
// harness adapts real OTel spans into this model, so the canonicalizer and
// everything downstream consume a stable shape and never import OTel
// (decision D8).
//
// This package owns three responsibilities that do not need a tracing backend:
// the Span / CapturedFlow data types, scoping a span set to one flow by its
// correlation key, and the deterministic sibling-ordering decision (the
// caller-clock concurrency signal of harness §3 / canon §3.3). Tree assembly and
// the rest of normalization belong to the canonicalizer.
package capture

import (
	"sort"
	"time"

	"github.com/jyang234/golang-code-graph/ir"
)

// TriggerKind is how a flow begins: an inbound HTTP request or a consumed event.
type TriggerKind string

const (
	TriggerHTTP  TriggerKind = "http"
	TriggerEvent TriggerKind = "event"
)

// CaptureMode is the capture topology (harness §1). v1 is in-process; post-hoc
// is the deferred out-of-process path.
type CaptureMode string

const (
	ModeInProcess CaptureMode = "in-process"
	ModePostHoc   CaptureMode = "post-hoc"
)

// Status mirrors a span's OTel status, normalized to the three values the IR
// retains.
const (
	StatusUnset = "unset"
	StatusOK    = "ok"
	StatusError = "error"
)

// Span is one captured operation in flowmap's internal model. Start and End are
// caller-clock timestamps used transiently to decide sibling ordering (§3.3) and
// then discarded by the canonicalizer; they are never serialized into a golden.
type Span struct {
	// TraceID identifies the span's trace. Out of process a flow can cross trace
	// boundaries (an async broker hand-off starts a new trace), so a span is
	// identified by (TraceID, ID) and a Link's target by (TraceID, SpanID).
	TraceID  string
	ID       string
	ParentID string
	Name     string
	Kind     ir.Kind
	Attrs    map[string]string

	Status    string // unset|ok|error
	ErrorType string // normalized exception class; "" if none

	Start time.Time
	End   time.Time

	// Links are the span's OTLP links — references to causally-related spans in
	// other traces. The async-flow membership signal across a broker (a FOLLOWS_FROM
	// from a consumer back to the producer it processes), where neither baggage nor
	// parent_span_id crosses. Empty for purely synchronous in-process flows.
	Links []SpanLink

	// AsyncLink marks a span that was reparented onto its parent across a broker by
	// following an OTLP span link (FOLLOWS_FROM), rather than by an in-trace
	// parent_span_id. Such a span is a separately-polled continuation caused by — not
	// synchronously called during — its parent's work, so the renderer draws the hop
	// into it as a distinct asynchronous interaction. Set by ingest.stitch; never set
	// for a synchronous, single-trace flow.
	AsyncLink bool

	// Goroutine is the id of the goroutine the span started on — the structural
	// concurrency signal (canon §3.3, plan [C2]). A child that runs on a different
	// goroutine than its parent was dispatched asynchronously; two such siblings
	// are a race regardless of how their intervals happen to fall. Zero means the
	// signal was unavailable, and ordering falls back to caller-clock overlap.
	Goroutine uint64
}

// SpanLink is a reference from one span to another (OTLP span link), identified by
// the linked span's (TraceID, SpanID).
type SpanLink struct {
	TraceID string
	SpanID  string
}

// CodeStampAttr is the OTel RESOURCE attribute carrying the deployed code
// identity (typically the commit SHA) of the emitting service — the behavioral
// mirror of the static graph's --stamp, matched by the behavioral-impeachment
// ladder's code-identity rung. It is flowmap-specific (no OTel semantic
// convention pins a deployed-commit resource attribute) and the ONE owner of the
// name, so ingest (post-hoc) and the in-process harness key it identically.
const CodeStampAttr = "flowmap.code.stamp"

// FQNTagKey is the OTel SPAN attribute carrying the runtime fully-qualified name
// of the first-party function that opened a span — the L1 capture tag the
// behavioral-impeachment severance walk reconciles to an ssa graph node (plan §7).
// It is flowmap-specific and the ONE owner of the name, so the in-process harness
// producer sets it and the impeach consumer (internal/impeach) reads it under the
// same key. Absent when the producer cannot confidently name a first-party frame
// (fail closed: an untagged span maps no node and the walk stays at L0).
const FQNTagKey = "flowmap.fqn"

// CaptureProvenanceAttr is the OTel RESOURCE attribute carrying the capture
// fidelity grade ("production" | "integration" | "synthetic") — the
// behavioral-impeachment ladder's capture-fidelity input (§4 rung 5, §12.6). It is
// flowmap-specific and the ONE owner of the name: a real deployment sets it on its
// resource (production), and ingest folds it onto CapturedFlow.Provenance, exactly
// as CodeStampAttr carries the code identity. Producer-set, never inferred — the
// corpus self-describes its grade rather than the audit caller asserting it.
const CaptureProvenanceAttr = "flowmap.capture.provenance"

// The capture fidelity grades carried by CaptureProvenanceAttr / CapturedFlow.
// Provenance — the ONE source of this vocabulary (impeach's ladder references
// these, so the producer and the consumer cannot drift). Only production and
// integration clear the impeachment capture-fidelity rung; synthetic and an
// unestablished grade fail it closed.
const (
	CaptureProduction  = "production"
	CaptureIntegration = "integration"
	CaptureSynthetic   = "synthetic"
)

// AssertableGrade reports whether g is a capture-fidelity grade a human may
// ASSERT (via `--capture`): only production and integration, the two that can
// clear the impeachment capture-fidelity rung. synthetic is producer-set and never
// asserted (asserting it could only ever cap a witness at CAPTURE-UNTRUSTED), and
// the empty string means "not asserted" — both fail this check, so a caller
// validates only grades a user actually typed. This is the ONE source both the
// verify CLI and the MCP server validate `--capture` against, so the two boundaries
// can never drift on what a valid assertion is (tenet 2: refuse a bad grade, never
// launder it into a silent CAPTURE-UNTRUSTED downgrade). Guarded by
// capture.TestAssertableGrade.
func AssertableGrade(g string) bool {
	return g == CaptureProduction || g == CaptureIntegration
}

// AgreedStamp is the ONE-SOURCE code-identity reduction: the single distinct
// NON-EMPTY stamp among the inputs, with ok=false when two non-empty stamps
// disagree. Empty inputs are skipped, so a result of ("", true) means "no stamp
// seen at all" — distinct from ("", false), a genuine disagreement. Both
// stamp-reducing consumers route their disagreement check through here so the
// fail-closed-on-conflict rule lives in one place (ingest.flowStamp over a
// service's spans, impeach.corpusIdentity over a corpus's traces); each layers
// its OWN empty-policy on top — a span MAY lack a stamp without vetoing, a trace
// MAY NOT — and that empty-policy is the only thing the two are permitted to
// differ on. Guarded by capture.TestAgreedStamp.
func AgreedStamp(stamps []string) (stamp string, ok bool) {
	for _, s := range stamps {
		if s == "" {
			continue
		}
		switch {
		case stamp == "":
			stamp = s
		case stamp != s:
			return "", false
		}
	}
	return stamp, true
}

// CapturedFlow is the harness's output and the canonicalizer's input
// (harness §7). Complete=false is a hard stop: the canonicalizer must refuse to
// snapshot a truncated trace.
type CapturedFlow struct {
	Flow    string
	Service string // the self lifeline (OTel resource service.name)
	// Stamp is the code identity (deployed commit) read from the CodeStampAttr
	// resource attribute (post-hoc) or set by the harness WithCodeStamp option
	// (in-process). It rides through to ir.CanonicalTrace.Stamp, where it is
	// excluded from snapshot equality. Empty when the capture carried no stamp.
	Stamp string
	// Provenance is the capture FIDELITY grade ("production" | "integration" |
	// "synthetic") read from the CaptureProvenanceAttr resource attribute (post-hoc)
	// or set by the in-process harness ("integration" — it runs the real code, never
	// "production"). It rides through to ir.CanonicalTrace.Provenance. Empty when the
	// capture declared no grade.
	Provenance string
	Trigger    TriggerKind
	Mode       CaptureMode
	Spans      []Span
	Root       *Span
	Complete   bool
}

// Attr returns the value of key on s, or "" if absent.
func (s *Span) Attr(key string) string {
	if s == nil || s.Attrs == nil {
		return ""
	}
	return s.Attrs[key]
}

// Scope returns the spans carrying the given correlation id in test.run.id,
// reconstructs the root, and reports whether the set is internally consistent.
// In-process capture gets scoping for free (the in-memory exporter only holds
// this run's spans), but filtering by the correlation attribute is the robust
// backstop the spec mandates (§3): it survives an instrumentation that starts a
// fresh trace on entry. A runID of "" keeps every span (the in-process fast
// path).
//
// The returned root is the single span whose parent is outside the scoped set
// (the reconstructed entry — a server or consumer span). Multiple such spans, or
// none, signal a scoping/completeness problem the caller surfaces via Complete;
// canonicalization re-checks and attaches orphans to a synthetic root.
func Scope(spans []Span, runID string) (scoped []Span, root *Span) {
	scoped = make([]Span, 0, len(spans))
	for _, s := range spans {
		if runID == "" || s.Attr(CorrelationKey) == runID {
			scoped = append(scoped, s)
		}
	}
	sortSpans(scoped)

	ids := make(map[string]bool, len(scoped))
	for i := range scoped {
		ids[scoped[i].ID] = true
	}
	for i := range scoped {
		if !ids[scoped[i].ParentID] {
			if root == nil {
				root = &scoped[i]
			} else {
				// More than one in-scope root: ambiguous. Return a nil root so the
				// caller's `root != nil` completeness check fails closed and the
				// ambiguous set is never snapshotted as if it had a single entry.
				return scoped, nil
			}
		}
	}
	return scoped, root
}

// CorrelationKey is the span attribute the harness copies from baggage so a span
// can be attributed to exactly one test run (§3).
const CorrelationKey = "test.run.id"

// sortSpans imposes a deterministic, run-independent order on the flat span set:
// by start time (so assembly and ordering see a stable sequence), then by id as
// a tiebreaker. Ordering of *siblings in the tree* is decided separately by
// Order; this is only to keep the flat list reproducible.
func sortSpans(spans []Span) {
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].Start.Equal(spans[j].Start) {
			return spans[i].Start.Before(spans[j].Start)
		}
		return spans[i].ID < spans[j].ID
	})
}

// Concurrent reports whether two sibling spans ran concurrently (canon §3.3,
// plan [C2]). It prefers the structural dispatch signal: when goroutine identity
// is known for both siblings and their parent, two siblings dispatched onto
// *distinct* worker goroutines (each different from the parent's goroutine and
// from each other) are a race regardless of how their intervals happen to fall —
// robust to scheduling jitter and to one leg finishing before the other starts.
// It falls back to caller-clock interval overlap when the signal is unavailable
// (parentGoroutine or either span's goroutine is zero), when at least one sibling
// ran inline on the parent's goroutine (its order is then the parent's execution
// order), or when both siblings ran on the *same* worker goroutine: same-goroutine
// spans are serialized by construction, so their order is a happens-before, never a
// race, and overlaps() is correctly false for them.
//
// All three spans share one process clock here, so interval comparison is sound
// — unlike cross-service server spans in separate clock domains, which must never
// be compared this way.
func Concurrent(a, b Span, parentGoroutine uint64) bool {
	if parentGoroutine != 0 && a.Goroutine != 0 && b.Goroutine != 0 {
		aAsync := a.Goroutine != parentGoroutine
		bAsync := b.Goroutine != parentGoroutine
		// a.Goroutine != b.Goroutine is load-bearing: two sequential spans on one
		// worker goroutine (both async, but equal to each other) are serialized, not
		// a race; classifying them Concurrent would let canon reorder them by
		// canonical key and erase their real happens-before order.
		if aAsync && bAsync && a.Goroutine != b.Goroutine {
			return true
		}
	}
	return overlaps(a, b)
}

// overlaps reports whether two spans' caller-clock intervals intersect.
func overlaps(a, b Span) bool {
	return a.Start.Before(b.End) && b.Start.Before(a.End)
}
