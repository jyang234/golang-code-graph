// Package await implements quiescence and completeness detection — the
// make-or-break of the capture harness (trace-capture-harness spec §4).
// Snapshotting before a trace is complete yields a false golden, the worst
// failure mode, so completion is decided by three signals in order of authority:
// expected-exit markers (the flow's declared I/O contract), a quiet-period
// backstop, and a hard timeout that fails loudly rather than snapshotting a
// partial trace.
//
// It is pure relative to its inputs: the span source and the clock are injected,
// so the loop is exercised deterministically in tests without real sleeping.
package await

import (
	"time"

	"github.com/jyang234/golang-code-graph/capture"
	"github.com/jyang234/golang-code-graph/ir"
)

// Options configures one await. Markers and Match together encode the flow's
// expected exits; Quiet and Timeout are the backstops; MinSpans is the sanity
// floor. Now and Sleep are injectable so the loop can be driven by a fake clock.
type Options struct {
	// Markers are the flow's declared expected-exit op keys (e.g.
	// "PUBLISH loan.approved"). Empty means completion rests on the quiet period
	// and root-ended signals alone.
	//
	// A fire-and-forget effect — an async span that lands AFTER the root has ended
	// — is only guaranteed to be captured if it is declared as a marker: without
	// one, an effect that fires after Quiet is dropped with Complete=true, and one
	// that straddles the Quiet boundary makes the golden itself flake (M-19). Every
	// flow with a detached goroutine / late publish must declare a marker for it;
	// quiet-alone completion is a backstop for synchronous flows, not a substitute.
	Markers []string
	// Match reports whether a span satisfies a marker. Callers pass the op-key
	// matcher so the marker grammar stays coupled to canonical op keys.
	Match func(capture.Span, string) bool

	Quiet    time.Duration // required idle interval after the last new span
	Timeout  time.Duration // hard deadline; exceeding it fails loudly
	Poll     time.Duration // sleep between polls; defaults to 5ms
	MinSpans int           // minimum span count sanity floor; defaults to 1

	Now   func() time.Time    // defaults to time.Now
	Sleep func(time.Duration) // defaults to time.Sleep
}

// Await polls snapshot (the currently-ended spans for this scope) until the flow
// is complete or the deadline passes. Completion requires, together: the root
// span has ended, every marker has been observed, no new span for the quiet
// interval, and at least MinSpans spans. It returns the final span set and
// whether the flow completed; complete=false means truncated — the caller must
// not snapshot it.
func Await(snapshot func() []capture.Span, opt Options) (spans []capture.Span, complete bool) {
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	sleep := opt.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	poll := opt.Poll
	if poll <= 0 {
		poll = 5 * time.Millisecond
	}
	minSpans := opt.MinSpans
	if minSpans < 1 {
		minSpans = 1
	}

	deadline := now().Add(opt.Timeout)
	lastCount := -1
	lastChange := now()

	for {
		spans = snapshot()
		if len(spans) != lastCount {
			lastCount = len(spans)
			lastChange = now()
		}
		if rootEnded(spans) &&
			allSeen(spans, opt.Markers, opt.Match) &&
			len(spans) >= minSpans &&
			now().Sub(lastChange) >= opt.Quiet {
			return spans, true
		}
		if now().After(deadline) {
			return spans, false // truncated — do not snapshot
		}
		sleep(poll)
	}
}

// rootEnded reports whether the flow's entry span has ended. The recorder only
// surfaces spans on End, so a span's presence means it ended — but a parentless
// span is not necessarily the root: while the flow is still running, a leaf
// whose intermediate parent has not yet ended is transiently parentless too.
// Keying completion on "any orphan" would fire mid-flow and snapshot a truncated
// trace (the worst failure mode). The flow's entry is always a server (HTTP) or
// consumer (event) span whose parent is outside the scoped set, so we require an
// ended span of that kind. A fire-and-forget goroutine span arriving after the
// root is still caught by the quiet-period drain.
func rootEnded(spans []capture.Span) bool {
	ids := make(map[string]bool, len(spans))
	for i := range spans {
		ids[spans[i].ID] = true
	}
	for i := range spans {
		s := &spans[i]
		if (s.Kind == ir.KindServer || s.Kind == ir.KindConsumer) && !ids[s.ParentID] {
			return true
		}
	}
	return false
}

// allSeen reports whether every marker is satisfied by some span.
func allSeen(spans []capture.Span, markers []string, match func(capture.Span, string) bool) bool {
	for _, m := range markers {
		found := false
		for i := range spans {
			if match != nil && match(spans[i], m) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
