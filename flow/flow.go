// Package flow is flowmap's PUBLIC test DSL: a service author declares a flow,
// triggers it through the harness, and asserts it against a committed golden —
// all inside a plain `go test` (trace-capture-harness spec, golden-diff spec,
// plan [H3]). It is the consumer-facing gate, and together with harness and ir
// forms flowmap's stable public surface (plan [C1]).
//
// Run wires the whole behavioral pipeline: trigger → capture → canonicalize →
// determinism self-test → golden compare (or -update) → cardinality. The
// cardinality check is enforced by the runner against the IR's observed
// multiplicity, independently of the snapshot equality, so a prescriptive
// invariant fails even when the golden matches. Expected-exit markers reuse the
// canonical op-key grammar, so "PUBLISH loan.approved" matches a canonical Op
// exactly.
package flow

import (
	"context"
	"time"

	"github.com/jyang234/golang-code-graph/harness"
	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/golden"
	"github.com/jyang234/golang-code-graph/ir"
)

// selfTestRuns is how many times Run re-drives the flow to prove the capture →
// canon transform is deterministic before it trusts the snapshot (canon §5).
const selfTestRuns = 3

// TB is the subset of *testing.T the DSL needs.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Flow is a declared flow under test, built fluently and run with Run.
type Flow struct {
	name    string
	dir     string
	tier    string
	quiet   time.Duration
	timeout time.Duration

	trigger func(*harness.App) *harness.Pending
	expect  []expectation
}

type expectation struct {
	op          string
	exactlyOnce bool
}

// New starts a flow declaration. name is the stable flow id; it is also the
// golden file stem.
func New(name string) *Flow {
	return &Flow{name: name, dir: "testdata/flows", tier: "warn"}
}

// Trigger drives the flow as an inbound HTTP request through the real router.
func (f *Flow) Trigger(method, target string) *Flow {
	f.trigger = func(a *harness.App) *harness.Pending { return a.HTTP(method, target, nil) }
	return f
}

// TriggerBody is Trigger with a request body.
func (f *Flow) TriggerBody(method, target string, body []byte) *Flow {
	f.trigger = func(a *harness.App) *harness.Pending { return a.HTTP(method, target, body) }
	return f
}

// TriggerEvent drives the flow as a consumed event: deliver runs the service's
// real consumer path against the injected, correlated context.
func (f *Flow) TriggerEvent(name string, deliver func(ctx context.Context)) *Flow {
	f.trigger = func(a *harness.App) *harness.Pending { return a.Event(name, deliver) }
	return f
}

// Expect declares an expected-exit op (the flow's I/O contract): it gates
// completion (the capture waits until the op is observed) but asserts no
// cardinality beyond at-least-once.
func (f *Flow) Expect(op string) *Flow {
	f.expect = append(f.expect, expectation{op: op})
	return f
}

// ExpectExactlyOnce declares an op that must occur exactly once. It gates
// completion and adds a prescriptive cardinality assertion enforced against the
// IR's observed multiplicity, independent of the golden.
func (f *Flow) ExpectExactlyOnce(op string) *Flow {
	f.expect = append(f.expect, expectation{op: op, exactlyOnce: true})
	return f
}

// Tier sets the salience threshold retained in the snapshot (warn|info|debug|all;
// default warn).
func (f *Flow) Tier(tier string) *Flow { f.tier = tier; return f }

// GoldensDir overrides where snapshots are read/written (default testdata/flows).
func (f *Flow) GoldensDir(dir string) *Flow { f.dir = dir; return f }

// Quiescence overrides the quiet interval and hard timeout for capture.
func (f *Flow) Quiescence(quiet, timeout time.Duration) *Flow {
	f.quiet, f.timeout = quiet, timeout
	return f
}

// Run executes the flow and gates it. It fails the test if the capture is
// truncated, if the transform is non-deterministic across re-runs, if the
// snapshot does not match the committed golden, or if a declared cardinality is
// violated. With `go test -update` it rebases the golden and rendered view.
func (f *Flow) Run(t TB, app *harness.App) {
	t.Helper()
	if f.trigger == nil {
		t.Fatalf("flow %q: no trigger declared (call Trigger/TriggerEvent)", f.name)
		return
	}

	cfg := &config.Config{Canon: config.CanonConfig{SalienceTier: f.tier}}
	markers := make([]string, 0, len(f.expect))
	for _, e := range f.expect {
		markers = append(markers, e.op)
	}

	traces := make([]*ir.CanonicalTrace, 0, selfTestRuns)
	for i := 0; i < selfTestRuns; i++ {
		cf, err := f.trigger(app).Capture(harness.CaptureOptions{
			Markers: markers,
			Quiet:   f.quiet,
			Timeout: f.timeout,
		})
		if err != nil {
			t.Fatalf("flow %q: capture failed on run %d: %v", f.name, i+1, err)
			return
		}
		tr, err := canon.Canonicalize(*cf, cfg)
		if err != nil {
			t.Fatalf("flow %q: canonicalization failed on run %d: %v", f.name, i+1, err)
			return
		}
		traces = append(traces, tr)
	}

	// Determinism self-test: every run must canonicalize byte-identically before
	// the snapshot is trusted (canon §5).
	base := marshal(traces[0])
	for i := 1; i < len(traces); i++ {
		if marshal(traces[i]) != base {
			t.Fatalf("flow %q: non-deterministic capture — run %d differs from run 1; "+
				"the flow or its test data is not yet deterministic", f.name, i+1)
			return
		}
	}

	if err := golden.Compare(traces[0], f.dir, f.name, golden.Update()); err != nil {
		t.Errorf("flow %q: %v", f.name, err)
	}

	// Cardinality is enforced against the observed IR independently of the golden,
	// so a prescriptive invariant fails even when the snapshot matches.
	for _, e := range f.expect {
		count, looped := countOp(traces[0].Root, e.op)
		if count == 0 {
			t.Errorf("flow %q: expected op %q was not observed", f.name, e.op)
			continue
		}
		if e.exactlyOnce && (count != 1 || looped) {
			detail := pluralCount(count)
			if looped {
				detail = "a collapsed loop (1..*)"
			}
			t.Errorf("flow %q: op %q declared ExpectExactlyOnce but observed %s", f.name, e.op, detail)
		}
	}
}

func marshal(t *ir.CanonicalTrace) string {
	b, _ := t.Marshal()
	return string(b)
}

// countOp returns how many times op occurs in the tree and whether any
// occurrence sits in a loop-collapsed group (observed multiplicity 1..*).
func countOp(s *ir.CanonicalSpan, op string) (count int, looped bool) {
	if s == nil {
		return 0, false
	}
	if s.Op == op {
		count++
	}
	for _, g := range s.Children {
		for _, m := range g.Members {
			if m.Op == op && g.Multiplicity == "1..*" {
				looped = true
			}
			c, l := countOp(m, op)
			count += c
			looped = looped || l
		}
	}
	return count, looped
}

func pluralCount(n int) string {
	if n == 1 {
		return "1 occurrence"
	}
	return itoa(n) + " occurrences"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}
