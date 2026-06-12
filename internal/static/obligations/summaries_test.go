package obligations

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// buildProg compiles one inline source file and returns every function in the
// program (anonymous functions and wrappers included) — the summary engine's
// universe must contain closures and bound-method wrappers, unlike the
// per-function tables' package filter.
func buildProg(t *testing.T, src string) []*ssa.Function {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg := types.NewPackage("example.com/fix", "")
	spkg, _, err := ssautil.BuildPackage(
		&types.Config{Importer: importer.Default()}, fset, pkg, []*ast.File{f},
		ssa.SanityCheckFunctions|ssa.InstantiateGenerics)
	if err != nil {
		t.Fatalf("build SSA: %v", err)
	}
	var fns []*ssa.Function
	for fn := range ssautil.AllFunctions(spkg.Prog) {
		fns = append(fns, fn)
	}
	return fns
}

// testUnit builds a Unit with a CHA-like resolver: static callees resolve to
// themselves; invoke-mode calls enumerate every universe method of the right
// name whose receiver implements the interface; dynamic function values do
// not resolve (a frontier) — the same over-approximation contract the
// production adapter inherits from the call graph.
func testUnit(fns []*ssa.Function) *Unit {
	return &Unit{Fns: fns, Callees: func(site ssa.CallInstruction) []*ssa.Function {
		common := site.Common()
		if common.IsInvoke() {
			iface, ok := common.Value.Type().Underlying().(*types.Interface)
			if !ok {
				return nil
			}
			var out []*ssa.Function
			for _, fn := range fns {
				if fn.Name() != common.Method.Name() || fn.Signature.Recv() == nil {
					continue
				}
				if types.Implements(fn.Signature.Recv().Type(), iface) {
					out = append(out, fn)
				}
			}
			return out
		}
		if sc := common.StaticCallee(); sc != nil {
			return []*ssa.Function{sc}
		}
		return nil
	}}
}

func fnByName(t *testing.T, fns []*ssa.Function, name string) *ssa.Function {
	t.Helper()
	for _, fn := range fns {
		if fn.Name() == name && fn.Parent() == nil {
			return fn
		}
	}
	t.Fatalf("function %s not in fixture", name)
	return nil
}

var releaseTargets = []string{"example.com/fix#Commit", "example.com/fix#Rollback"}

const dischargeSrc = `package fix

type Tx struct{ closed bool }

func (t *Tx) Commit() error { t.closed = true; return nil }
func (t *Tx) Rollback()     { t.closed = true }

func work(t *Tx) error { return nil }

// ALWAYS: releases on every path.
func finish(t *Tx, failed bool) error {
	if failed {
		t.Rollback()
		return nil
	}
	return t.Commit()
}

// ALWAYS through one more level: composition over the DAG.
func finishNested(t *Tx, failed bool) error { return finish(t, failed) }

// One arm only: not ALWAYS, and the target keeps it from NEVER.
func finishLeaky(t *Tx, failed bool) error {
	if failed {
		t.Rollback()
	}
	return nil
}

// NEVER: the cone is closed and target-free.
func helper(t *Tx) error { return work(t) }

// NEVER survives recursion: reachability needs no CFG induction.
func recurseNever(t *Tx, n int) {
	if n > 0 {
		recurseNever(t, n-1)
	}
}

// Cyclic SCC with a target in the cone: UNKNOWN, no fixed point.
func recurseRel(t *Tx, n int) {
	if n == 0 {
		t.Rollback()
		return
	}
	recurseRel(t, n-1)
}

// recover makes the CFG untrustworthy for ALWAYS.
func finishRecover(t *Tx) error {
	defer func() { _ = recover() }()
	return t.Commit()
}

// The deferReleases named-helper ceiling, lifted: cleanup is ALWAYS, so the
// defer covers — without naming cleanup as a release ref.
func cleanup(t *Tx) { t.Rollback() }
func finishDeferred(t *Tx) error {
	defer cleanup(t)
	return work(t)
}

// An uncovered explicit panic is an exit.
func finishPanics(t *Tx, failed bool) error {
	if failed {
		panic("boom")
	}
	return t.Commit()
}

// A dynamic call is a frontier, but coverage after it still proves ALWAYS —
// the field RunInTx shape: interface/closure below the release, never
// between acquire and exit.
func RunInTx(t *Tx, fn func(*Tx) error) error {
	if err := fn(t); err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}

// A frontier with no visible target: the cone is open, so not NEVER.
func dynOnly(fn func()) { fn() }
`

func TestDischarges(t *testing.T) {
	fns := buildProg(t, dischargeSrc)
	s := NewSummaries(testUnit(fns))
	cases := []struct {
		fn   string
		want Summary
	}{
		{"finish", SummaryAlways},
		{"finishNested", SummaryAlways},
		{"finishDeferred", SummaryAlways},
		{"RunInTx", SummaryAlways},
		{"helper", SummaryNever},
		{"work", SummaryNever},
		{"recurseNever", SummaryNever},
		{"finishLeaky", SummaryUnknown},
		{"recurseRel", SummaryUnknown},
		{"finishRecover", SummaryUnknown},
		{"finishPanics", SummaryUnknown},
		{"dynOnly", SummaryUnknown},
	}
	for _, c := range cases {
		if got := s.Discharges(fnByName(t, fns, c.fn), releaseTargets); got != c.want {
			t.Errorf("Discharges(%s) = %s, want %s", c.fn, got, c.want)
		}
	}
}

const invokeGoodSrc = `package fix

type Tx struct{ closed bool }

func (t *Tx) Rollback() { t.closed = true }

type Finisher interface{ Done(t *Tx) }

type GoodF struct{}

func (GoodF) Done(t *Tx) { t.Rollback() }

func viaDone(f Finisher, t *Tx) { f.Done(t) }
`

const invokeMixedSrc = invokeGoodSrc + `
type BadF struct{}

func (BadF) Done(t *Tx) {}
`

// An invoke-mode call earns credit only when every candidate in the
// over-approximated set discharges; one silent implementation breaks the
// proof.
func TestDischargesInvokeCandidates(t *testing.T) {
	rollback := []string{"example.com/fix#Rollback"}

	good := buildProg(t, invokeGoodSrc)
	if got := NewSummaries(testUnit(good)).Discharges(fnByName(t, good, "viaDone"), rollback); got != SummaryAlways {
		t.Errorf("viaDone (sole conforming impl) = %s, want ALWAYS", got)
	}

	mixed := buildProg(t, invokeMixedSrc)
	if got := NewSummaries(testUnit(mixed)).Discharges(fnByName(t, mixed, "viaDone"), rollback); got != SummaryUnknown {
		t.Errorf("viaDone (mixed impls) = %s, want UNKNOWN", got)
	}
}

const requireRef = "example.com/fix#ValidatePayload"

const entrySrc = `package fix

func ValidatePayload() error { return nil }
func Publish()               {}

// Every entry dominated directly: the field doPublish→publishWithFanout shape.
func pfDominated() { Publish() }
func doPublish() {
	if ValidatePayload() == nil {
		pfDominated()
	}
}

// One additional caller with no require behind it: a proven open entry.
func pfOpen() { Publish() }
func doPublishOpen() {
	if ValidatePayload() == nil {
		pfOpen()
	}
}
func openCaller() { pfOpen() }

// Dominated one level up: the caller's own entries are all dominated.
func pfChain() { Publish() }
func mid()     { pfChain() }
func top() {
	if ValidatePayload() == nil {
		mid()
	}
}

// Derived A: validateAll ALWAYS-calls the require.
func pfDerived()  { Publish() }
func validateAll() { _ = ValidatePayload() }
func doPublishDerived() {
	validateAll()
	pfDerived()
}

// A deferred require runs at exit, after the entry it must precede.
func pfDeferredReq() { Publish() }
func doPublishDeferred() {
	defer ValidatePayload()
	pfDeferredReq()
}

// The function's address is taken: an invisible caller may exist.
func pfTaken() { Publish() }

var sink = pfTaken

// Recursion among the callers: abstain.
func pfRec() { Publish() }
func ra(n int) {
	if n > 0 {
		rb(n - 1)
	}
	pfRec()
}
func rb(n int) { ra(n) }
`

func TestEntryDominated(t *testing.T) {
	fns := buildProg(t, entrySrc)
	s := NewSummaries(testUnit(fns))
	cases := []struct {
		fn   string
		want Summary
	}{
		{"pfDominated", SummaryAlways},
		{"pfChain", SummaryAlways},
		{"pfDerived", SummaryAlways},
		{"pfOpen", SummaryNever},        // openCaller is a source with no require
		{"pfDeferredReq", SummaryNever}, // a deferred require does not precede
		{"doPublish", SummaryNever},     // a graph source is entered bare
		{"pfTaken", SummaryUnknown},
		{"pfRec", SummaryUnknown},
	}
	for _, c := range cases {
		if got := s.EntryDominated(fnByName(t, fns, c.fn), requireRef); got != c.want {
			t.Errorf("EntryDominated(%s) = %s, want %s", c.fn, got, c.want)
		}
	}
}

// The engine's answers are a pure function of the unit: input order must not
// matter (the universe is sorted, SCC identity derived from the sorted
// adjacency, edge folds order-independent).
func TestSummariesOrderIndependence(t *testing.T) {
	fns := buildProg(t, dischargeSrc)
	rev := make([]*ssa.Function, len(fns))
	for i, fn := range fns {
		rev[len(fns)-1-i] = fn
	}
	a, b := NewSummaries(testUnit(fns)), NewSummaries(testUnit(rev))
	for _, fn := range fns {
		if fn.Parent() != nil {
			continue
		}
		ga, gb := a.Discharges(fn, releaseTargets), b.Discharges(fn, releaseTargets)
		if ga != gb {
			t.Errorf("Discharges(%s): %s with sorted input, %s with reversed", fn.Name(), ga, gb)
		}
	}

	efns := buildProg(t, entrySrc)
	erev := make([]*ssa.Function, len(efns))
	for i, fn := range efns {
		erev[len(efns)-1-i] = fn
	}
	ea, eb := NewSummaries(testUnit(efns)), NewSummaries(testUnit(erev))
	for _, fn := range efns {
		if fn.Parent() != nil {
			continue
		}
		ga, gb := ea.EntryDominated(fn, requireRef), eb.EntryDominated(fn, requireRef)
		if ga != gb {
			t.Errorf("EntryDominated(%s): %s with sorted input, %s with reversed", fn.Name(), ga, gb)
		}
	}
}
