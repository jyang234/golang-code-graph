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
	"os"
	"strconv"
	"time"

	"github.com/jyang234/golang-code-graph/harness"
	"github.com/jyang234/golang-code-graph/internal/canon"
	"github.com/jyang234/golang-code-graph/internal/config"
	"github.com/jyang234/golang-code-graph/internal/golden"
	"github.com/jyang234/golang-code-graph/ir"
)

// defaultSelfTestRuns is how many times Run re-drives the flow to prove the
// capture → canon transform is deterministic before it trusts the snapshot
// (canon §5). Re-driving — rather than re-canonicalizing one capture — is what
// varies goroutine scheduling, which is the determinism risk worth catching.
//
// Because Run executes the whole flow this many times, the flow's side effects
// (DB writes, publishes) happen this many times: a flow must be idempotent or
// re-seed its fixtures per run, or set SelfTest(1) to opt out (trading
// scheduling-variation coverage for a single execution).
const defaultSelfTestRuns = 3

// TB is the subset of *testing.T the DSL needs.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Flow is a declared flow under test, built fluently and run with Run.
type Flow struct {
	name      string
	dir       string
	tier      string
	quiet     time.Duration
	timeout   time.Duration
	selfTest  int
	configDir string

	trigger func(*harness.App) *harness.Pending
	expect  []expectation
}

type expectation struct {
	op          string
	exactlyOnce bool
}

// New starts a flow declaration. name is the stable flow id; it is also the
// golden file stem. tier is left unset so a service's .flowmap.yaml salience
// threshold applies unless the flow overrides it with Tier.
func New(name string) *Flow {
	return &Flow{name: name, dir: "testdata/flows", selfTest: defaultSelfTestRuns}
}

// SelfTest sets how many times Run re-drives the flow for the determinism
// self-test (default 3). Use SelfTest(1) for a flow whose side effects are not
// idempotent and not re-seeded per run; this trades scheduling-variation
// coverage for a single execution. Values below 1 are treated as 1.
func (f *Flow) SelfTest(n int) *Flow {
	if n < 1 {
		n = 1
	}
	f.selfTest = n
	return f
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
//
// Declaring a marker is REQUIRED for any fire-and-forget effect — an async span
// (detached goroutine, late publish) that lands after the request handler returns.
// Without an Expect for it, completion may be decided by the quiet period before
// the effect fires, dropping it from the golden with Complete=true, or the effect
// may straddle the quiet boundary and flake the golden run-to-run (M-19). Expect
// the late op so the capture waits for it deterministically.
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

// Tier overrides the salience threshold retained in the snapshot
// (warn|info|debug|all). Unset, the service's .flowmap.yaml salience setting (or
// the warn default) applies.
func (f *Flow) Tier(tier string) *Flow { f.tier = tier; return f }

// GoldensDir overrides where snapshots are read/written (default testdata/flows).
func (f *Flow) GoldensDir(dir string) *Flow { f.dir = dir; return f }

// ConfigDir sets the directory holding the service's .flowmap.yaml. Unset, the
// file is discovered by walking up from the working directory (like go.mod). It
// carries the same tier rules, pins, and canon knobs the static pipeline uses,
// so canonicalization here tiers spans identically — the tier-map is one
// classifier across both pipelines.
func (f *Flow) ConfigDir(dir string) *Flow { f.configDir = dir; return f }

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

	cfg := f.resolveConfig(t)
	markers := make([]string, 0, len(f.expect))
	for _, e := range f.expect {
		markers = append(markers, e.op)
	}

	runs := f.selfTest
	if runs < 1 {
		runs = defaultSelfTestRuns
	}
	traces := make([]*ir.CanonicalTrace, 0, runs)
	for i := 0; i < runs; i++ {
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
	base, err := marshal(traces[0])
	if err != nil {
		t.Fatalf("flow %q: marshal failed: %v", f.name, err)
		return
	}
	for i := 1; i < len(traces); i++ {
		cur, err := marshal(traces[i])
		if err != nil {
			t.Fatalf("flow %q: marshal failed on run %d: %v", f.name, i+1, err)
			return
		}
		if cur != base {
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

// resolveConfig loads the service's .flowmap.yaml through the same package the
// static boundary pipeline uses (config.LoadDir / config.Discover), so
// canonicalization applies the identical tier rules, pins, and canon knobs — the
// tier-map is one classifier across both pipelines. The per-flow Tier overrides
// the config's salience threshold. A missing file yields defaults; an unreadable
// or malformed one is a hard error.
func (f *Flow) resolveConfig(t TB) *config.Config {
	cfg := &config.Config{}
	if dir, ok := f.configSearchDir(); ok {
		loaded, err := config.LoadDir(dir)
		if err != nil {
			t.Fatalf("flow %q: %v", f.name, err)
			return cfg
		}
		cfg = loaded
	}
	if f.tier != "" {
		// Validate the override against the SAME vocabulary config.Load enforces on
		// the file path. Without this, an unknown name (a typo like "Info" or
		// "trace") silently degrades to the warn threshold — a public-API fail-open
		// that contradicts tenet 2, while the config-file path hard-errors on the
		// identical typo. Fail loudly here too.
		if !config.ValidSalienceTier(f.tier) {
			t.Fatalf("flow %q: Tier(%q) not one of %s", f.name, f.tier, config.SalienceTierNames())
			return cfg
		}
		cfg.Canon.SalienceTier = f.tier // per-flow override
	}
	return cfg
}

// configSearchDir reports the directory to load .flowmap.yaml from: the explicit
// ConfigDir if set, else module-bounded discovery walking up from the working
// directory. config.Discover stops at the enclosing module root (go.mod), so a
// stray .flowmap.yaml in a parent module, the repo root, or the developer's home
// directory is never silently applied to a flow that did not opt into it.
func (f *Flow) configSearchDir() (string, bool) {
	if f.configDir != "" {
		return f.configDir, true
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	return config.Discover(wd)
}

// marshal renders the canonical bytes the determinism self-test compares. The
// error must propagate: swallowed, every run would serialize to "" and the
// self-test would pass vacuously.
func marshal(t *ir.CanonicalTrace) (string, error) {
	b, err := t.Marshal()
	return string(b), err
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
	return strconv.Itoa(n) + " occurrences"
}
