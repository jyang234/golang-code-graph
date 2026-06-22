package rebind_test

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
	"github.com/jyang234/golang-code-graph/internal/static/rebind"
)

func analyzeFixture(t *testing.T, name string) *analyze.Result {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", name)
	res, err := analyze.Analyze(dir, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}
	return res
}

// reaches reports whether the forward reach of fqn in g crosses a boundary effect whose
// label contains marker.
func reaches(g *graphio.Graph, fqn, marker string) bool {
	seen := map[string]bool{fqn: true}
	for changed := true; changed; {
		changed = false
		for _, e := range g.Edges {
			if seen[e.From] && !seen[e.To] {
				seen[e.To] = true
				changed = true
			}
		}
	}
	for to := range seen {
		if strings.Contains(to, marker) {
			return true
		}
	}
	return false
}

const (
	cmdA = "(*example.com/unionsvc.CmdA).Handle"
	cmdB = "(*example.com/unionsvc.CmdB).Handle"
	leak = "(*example.com/unionsvc.LeakCmd).Handle"
	hub  = "example.com/unionsvc.invoke"
)

// Before --rebind, the context-insensitive runner unions every closure: CmdA appears to
// issue CmdB's write. After de-union each command reaches ONLY its own write — the
// over-report is removed in the precision-improving direction.
func TestRebindDeUnionsConfinedClosures(t *testing.T) {
	res := analyzeFixture(t, "unionsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Precondition: the union is present — CmdA reaches BOTH writes.
	if !reaches(g, cmdA, "table_a") || !reaches(g, cmdA, "table_b") {
		t.Fatalf("precondition: CmdA should reach the unioned table_a AND table_b before rebind")
	}

	added, removed := graphio.ApplyRebind(g, res)
	if added == 0 || removed == 0 {
		t.Fatalf("rebind must add and remove edges, got +%d -%d", added, removed)
	}
	taggedFound := false
	for _, e := range g.Edges {
		if e.Via == rebind.Via {
			taggedFound = true
		}
	}
	if !taggedFound {
		t.Error("rebound edges must carry their via=rebind provenance")
	}

	// After de-union: each confined command reaches only its OWN write.
	if !reaches(g, cmdA, "table_a") {
		t.Error("CmdA must still reach its own write table_a after rebind")
	}
	if reaches(g, cmdA, "table_b") {
		t.Error("CmdA must NOT reach CmdB's write table_b after de-union (the over-report removed)")
	}
	if !reaches(g, cmdB, "table_b") || reaches(g, cmdB, "table_a") {
		t.Error("CmdB must reach only its own write table_b after de-union")
	}
}

// The SOUNDNESS guard: LeakCmd's closure ESCAPES (passed to RunInTx AND the helper
// invoke), so its union must be KEPT. The adversarial regression is the gate flip: a
// must_not_reach(invoke → table_leak) gate measured VIOLATED→false-PASS when an early
// per-closure de-union wrongly dropped the escaped closure. Here the helper invoke MUST
// still reach table_leak after rebind, so the gate stays VIOLATED.
func TestRebindKeepsEscapedClosureUnion(t *testing.T) {
	res := analyzeFixture(t, "unionsvc")

	// The plan must not touch the escaped closure (no add, no remove naming LeakCmd).
	plan := rebind.Compute(res)
	for _, e := range plan.Add {
		if strings.Contains(e.From, "LeakCmd") || strings.Contains(e.To, "LeakCmd") {
			t.Errorf("escaped closure was de-unioned (add): %s -> %s", e.From, e.To)
		}
	}
	for _, r := range plan.Remove {
		if strings.Contains(r[1], "LeakCmd") {
			t.Errorf("escaped closure's union edge was removed: %s -> %s", r[0], r[1])
		}
	}

	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reaches(g, hub, "table_leak") {
		t.Fatalf("precondition: the helper invoke should reach table_leak before rebind")
	}
	graphio.ApplyRebind(g, res)
	// The gate-flip repair: the helper still reaches the escaped write, so a
	// must_not_reach(invoke → table_leak) stays VIOLATED (no false absence).
	if !reaches(g, hub, "table_leak") {
		t.Error("after rebind the helper invoke must STILL reach table_leak — dropping it is a false absence (gate flip)")
	}
	// LeakCmd itself still reaches its write.
	if !reaches(g, leak, "table_leak") {
		t.Error("LeakCmd must still reach its own write")
	}
}

// Across every escape MODE — stored, returned, channel-sent, captured — the closure is
// not confined, so the pass abstains: the plan over escapemodessvc is EMPTY (no add, no
// remove). This is the conservatively-complete confinement guard exercised mode by mode.
func TestRebindAbstainsOnEveryEscapeMode(t *testing.T) {
	plan := rebind.Compute(analyzeFixture(t, "escapemodessvc"))
	if len(plan.Add) != 0 || len(plan.Remove) != 0 {
		t.Errorf("every closure here escapes; the de-union plan must be empty, got add=%v remove=%v", plan.Add, plan.Remove)
	}
}

// The plan and the applied graph must be byte-identical across repeated builds — the
// de-union is a pure function of the SSA (CLAUDE.md determinism). A non-deterministic
// add/remove order or a map-iteration leak would surface here.
func TestRebindDeterministic(t *testing.T) {
	// ifaceunionsvc included: the interface case derives runner FQNs from
	// prog.RuntimeTypes() (impl iteration order), so a missing canonical sort would
	// surface here as a flapping Remove order.
	for _, fixture := range []string{"unionsvc", "ifaceunionsvc"} {
		want := rebind.Compute(analyzeFixture(t, fixture))
		for i := 0; i < 20; i++ {
			got := rebind.Compute(analyzeFixture(t, fixture))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("rebind plan for %s not deterministic across runs:\n want %+v\n got  %+v", fixture, want, got)
			}
		}
	}
}

// Opt-in / not-default: ApplyRebind is the only graph mutation that removes edges, so the
// default Build must leave the union intact (a committed-golden-preserving invariant).
func TestRebindIsOptInOnly(t *testing.T) {
	res := analyzeFixture(t, "unionsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, e := range g.Edges {
		if e.Via == rebind.Via {
			t.Errorf("a default Build must not carry rebound edges: %+v", e)
		}
	}
	// The union edge is still present by default (CmdA over-reports until --rebind).
	if !reaches(g, cmdA, "table_b") {
		t.Error("default Build must keep the union (CmdA reaches table_b) — de-union is opt-in")
	}
}

const (
	ifaceCmdA = "(*example.com/ifaceunionsvc.CmdA).Handle"
	ifaceCmdB = "(*example.com/ifaceunionsvc.CmdB).Handle"
	ifaceCmdC = "(*example.com/ifaceunionsvc.CmdC).Handle"
	ifaceHub  = "example.com/ifaceunionsvc.invoke"
)

// The dominant real shape: the runner is held as an INTERFACE, so the union forms at the
// concrete impl's fn(exec) site. De-union must resolve the interface method to its impl,
// ADD each command's own edge, and REMOVE the impl→closure union edges — so each confined
// command reaches only its own write. Parity with the static-runner de-union.
func TestRebindDeUnionsInterfaceRunner(t *testing.T) {
	res := analyzeFixture(t, "ifaceunionsvc")
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Precondition: the interface union is present — CmdA reaches BOTH writes.
	if !reaches(g, ifaceCmdA, "table_a") || !reaches(g, ifaceCmdA, "table_b") {
		t.Fatalf("precondition: CmdA should reach the unioned table_a AND table_b before rebind")
	}

	added, removed := graphio.ApplyRebind(g, res)
	if added == 0 || removed == 0 {
		t.Fatalf("interface rebind must add and remove edges, got +%d -%d", added, removed)
	}
	if !reaches(g, ifaceCmdA, "table_a") {
		t.Error("CmdA must still reach its own write table_a after rebind")
	}
	if reaches(g, ifaceCmdA, "table_b") {
		t.Error("CmdA must NOT reach CmdB's write table_b after de-union (the interface over-report removed)")
	}
	if !reaches(g, ifaceCmdB, "table_b") || reaches(g, ifaceCmdB, "table_a") {
		t.Error("CmdB must reach only its own write table_b after de-union")
	}
}

// The soundness-dangerous REMOVE path on a VALUE-receiver interface runner: CmdC dispatches
// through ValueRunner (value receiver), whose go/ssa *ValueRunner promotion wrapper does not
// itself invoke the parameter. De-union must still resolve that wrapper to (ValueRunner).RunInTx
// (DirectlyInvokesParam), REMOVE the value method's union edge, and ADD CmdC's own edge — so
// CmdC reaches only table_c. Because ValueRunner shares TxRunner with *SQLRunner, this also
// pins the CONTAGION repair on the remove side: the wrapper must NOT make the shared resolution
// abstain (which would suppress de-union for CmdA/CmdB too). Differential: without the
// unwrapPromotion fix runnersToDeUnion abstains interface-wide and every assertion here fails.
func TestRebindDeUnionsValueReceiverInterfaceRunner(t *testing.T) {
	res := analyzeFixture(t, "ifaceunionsvc")

	// The value method's union→closure edge for CmdC is among the planned removals — proving the
	// remove path reaches the VALUE receiver (not the unrelated *ValueRunner wrapper).
	plan := rebind.Compute(res)
	removedValueRunnerForC := false
	for _, r := range plan.Remove {
		if strings.Contains(r[0], "ifaceunionsvc.ValueRunner") && !strings.Contains(r[0], "*") &&
			strings.Contains(r[1], "CmdC") {
			removedValueRunnerForC = true
		}
	}
	if !removedValueRunnerForC {
		t.Errorf("value-receiver de-union must remove (ValueRunner).RunInTx→CmdC closure; got removals %v", plan.Remove)
	}

	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Precondition: the shared interface union over-reports — CmdC reaches another command's write.
	if !reaches(g, ifaceCmdC, "table_c") || !reaches(g, ifaceCmdC, "table_a") {
		t.Fatalf("precondition: CmdC should reach its own table_c AND the unioned table_a before rebind")
	}

	added, removed := graphio.ApplyRebind(g, res)
	if added == 0 || removed == 0 {
		t.Fatalf("value-receiver interface rebind must add and remove edges, got +%d -%d", added, removed)
	}
	if !reaches(g, ifaceCmdC, "table_c") {
		t.Error("CmdC must still reach its own write table_c after rebind")
	}
	if reaches(g, ifaceCmdC, "table_a") || reaches(g, ifaceCmdC, "table_b") {
		t.Error("CmdC must NOT reach a sibling's write after de-union (the value-receiver over-report removed)")
	}
	// Contagion guard: the value-receiver impl must not have suppressed the pointer-receiver
	// siblings' de-union.
	if reaches(g, ifaceCmdA, "table_b") || reaches(g, ifaceCmdB, "table_a") {
		t.Error("contagion: a value-receiver impl must not suppress de-union of the pointer-receiver siblings")
	}
}

// The soundness guards carry over to interface dispatch:
//   - LeakCmd's closure ESCAPES (runner + helper) → union kept; the helper still reaches
//     table_leak, so a must_not_reach(invoke → table_leak) stays VIOLATED.
//   - MaybeCmd dispatches through a SECOND interface with a LAZY impl that never invokes
//     the closure → the de-union must ABSTAIN (not every impl directly invokes), so the
//     closure could be invoked on a path the removal would otherwise drop.
func TestRebindInterfaceKeepsEscapedAndMultiImpl(t *testing.T) {
	res := analyzeFixture(t, "ifaceunionsvc")
	plan := rebind.Compute(res)
	for _, e := range plan.Add {
		if strings.Contains(e.From, "LeakCmd") || strings.Contains(e.From, "MaybeCmd") {
			t.Errorf("a guarded command was de-unioned (add): %s -> %s", e.From, e.To)
		}
	}
	for _, r := range plan.Remove {
		if strings.Contains(r[1], "LeakCmd") || strings.Contains(r[1], "MaybeCmd") {
			t.Errorf("a guarded closure's union edge was removed: %s -> %s", r[0], r[1])
		}
		// The lazy impl's method must never be a removal target.
		if strings.Contains(r[0], "LazyRunner") || strings.Contains(r[0], "EagerRunner") {
			t.Errorf("MaybeRunner (eager/lazy) must not be de-unioned at all: %s -> %s", r[0], r[1])
		}
	}

	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !reaches(g, ifaceHub, "table_leak") {
		t.Fatalf("precondition: helper invoke should reach table_leak before rebind")
	}
	graphio.ApplyRebind(g, res)
	if !reaches(g, ifaceHub, "table_leak") {
		t.Error("after interface rebind the helper invoke must STILL reach table_leak (no false absence)")
	}
}
