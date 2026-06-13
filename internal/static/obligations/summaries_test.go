package obligations

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/jyang234/golang-code-graph/internal/config"
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

// Adversarial review F1: a deferred closure that releases CONDITIONALLY must
// not earn ALWAYS — the closure is judged by its own all-paths summary.
func finishMaybe(t *Tx, ok bool) error {
	defer func() {
		if ok {
			t.Rollback()
		}
	}()
	return nil
}

// The unconditional deferred closure keeps its credit the same way.
func finishClosure(t *Tx) error {
	defer func() { t.Rollback() }()
	return nil
}
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
		{"finishMaybe", SummaryUnknown},  // F1: conditional deferred closure
		{"finishClosure", SummaryAlways}, // the closure's own summary credits
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

// One additional caller with no require on the path: the entries are beyond
// proof (F3: there is no NEVER pole — out-of-unit or init callers may exist).
func pfOpen() { Publish() }
func doPublishOpen() {
	if ValidatePayload() == nil {
		pfOpen()
	}
}
func openCaller() { pfOpen() }

// F3a: two requires on the arms of a branch cover the join without either
// dominating it — coverage, not dominance, is the property execution has.
func pfBoth() { Publish() }
func callerBoth(c bool) {
	if c {
		_ = ValidatePayload()
	} else {
		_ = ValidatePayload()
	}
	pfBoth()
}

// F2: an unresolved invoke of Do exists (Hidden has no implementation in the
// universe), so any method named Do has a possible unseen entry.
type Doer struct{}

func (Doer) Do() { Publish() }

func directDo(d Doer) {
	if ValidatePayload() == nil {
		d.Do()
	}
}

type Hidden interface{ Do(int) }

func hiddenDo(h Hidden) { h.Do(1) }

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
		{"pfBoth", SummaryAlways},         // F3a: branch-covering requires
		{"pfOpen", SummaryUnknown},        // F3: no NEVER pole — beyond proof
		{"pfDeferredReq", SummaryUnknown}, // a deferred require does not precede
		{"doPublish", SummaryUnknown},     // a graph source: entries beyond proof
		{"pfTaken", SummaryUnknown},
		{"pfRec", SummaryUnknown},
	}
	for _, c := range cases {
		if got := s.EntryDominated(fnByName(t, fns, c.fn), requireRef); got != c.want {
			t.Errorf("EntryDominated(%s) = %s, want %s", c.fn, got, c.want)
		}
	}

	// F2: (Doer).Do is entered only via a covered static call, but the
	// unresolved invoke of a same-named method forces abstention.
	for _, fn := range fns {
		if fn.Name() == "Do" && fn.Signature.Recv() != nil && fn.Synthetic == "" {
			if got, note := s.EntryDominatedNote(fn, requireRef); got != SummaryUnknown || !strings.Contains(note, "unresolved interface dispatch") {
				t.Errorf("EntryDominated((Doer).Do) = %s (%s), want UNKNOWN via the F2 guard", got, note)
			}
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

// ---- CX-2: the must-precede lift through Check ---------------------------------

const liftSrc = `package fix

func Validate() error { return nil }
func Send(s string)   {}

// The field doPublish→publishWithFanout split: B sites one frame below the
// validation, every entry dominated.
func fanout()   { Send("a"); Send("b") }
func dispatch() {
	if Validate() == nil {
		fanout()
	}
}

// A second helper entered require-less from a graph source.
func fanoutOpen() { Send("a") }
func open()       { fanoutOpen() }

// Address taken: an unseen dynamic caller may exist.
func fanoutTaken() { Send("a") }

var hook = fanoutTaken

// Derived A: validateAll ALWAYS-calls the require, so its call site dominates.
func validateAll()     { _ = Validate() }
func fanoutDerived()   { Send("a") }
func dispatchDerived() {
	validateAll()
	fanoutDerived()
}

// Intraprocedural shapes must be untouched by the lift.
func direct() {
	_ = Validate()
	Send("x")
}
func directRacy(b bool) {
	if b {
		_ = Validate()
	}
	Send("x")
}
`

func liftRules() []config.ObligationRule {
	return []config.ObligationRule{
		{Name: "guard", Require: "example.com/fix#Validate", Before: "example.com/fix#Send", FromCallers: true},
		{Name: "pairing", Require: "example.com/fix#Validate", Before: "example.com/fix#Send"},
	}
}

func findingsOf(fs []Finding, rule, fn string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Rule == rule && strings.HasSuffix(f.Fn, "."+fn) {
			out = append(out, f)
		}
	}
	return out
}

func TestPrecedeLift(t *testing.T) {
	fns := buildProg(t, liftSrc)
	sums := NewSummaries(testUnit(fns))
	var pkgFns []*ssa.Function
	for _, fn := range fns {
		if fn.Pkg != nil && fn.Pkg.Pkg.Path() == "example.com/fix" && fn.Parent() == nil {
			pkgFns = append(pkgFns, fn)
		}
	}
	fs := Check(liftRules(), pkgFns, "", sums)

	type row struct {
		rule, fn string
		n        int
		want     Status
	}
	rows := []row{
		// The guard rule (fromCallers): the lift applies.
		{"guard", "fanout", 2, Satisfied},     // entry-covered via dispatch
		{"guard", "fanoutOpen", 1, CantProve}, // F3: entries beyond proof, never a borrowed witness
		{"guard", "fanoutTaken", 1, CantProve},
		{"guard", "fanoutDerived", 1, Satisfied}, // derived A covers in dispatchDerived
		{"guard", "direct", 1, Satisfied},
		{"guard", "directRacy", 1, CantProve}, // a graph source: the rule opted into caller context
		// The pairing rule (no fromCallers): today's semantics exactly.
		{"pairing", "fanout", 2, Violated},
		{"pairing", "fanoutOpen", 1, Violated},
		{"pairing", "fanoutTaken", 1, Violated},
		{"pairing", "fanoutDerived", 1, Violated},
		{"pairing", "direct", 1, Satisfied},
		{"pairing", "directRacy", 1, Violated},
	}
	for _, r := range rows {
		got := findingsOf(fs, r.rule, r.fn)
		if len(got) != r.n {
			t.Errorf("%s/%s: %d findings, want %d: %v", r.rule, r.fn, len(got), r.n, got)
			continue
		}
		for _, f := range got {
			if f.Status != r.want {
				t.Errorf("%s/%s = %s (%s), want %s", r.rule, r.fn, f.Status, f.Detail, r.want)
			}
		}
	}

	// The witnesses are part of the disclosure contract.
	if open := findingsOf(fs, "guard", "fanoutOpen"); len(open) == 1 && !strings.Contains(open[0].Detail, "open") {
		t.Errorf("fanoutOpen detail should name the unproven entry, got %q", open[0].Detail)
	}
	if taken := findingsOf(fs, "guard", "fanoutTaken"); len(taken) == 1 && !strings.Contains(taken[0].Detail, "address is taken") {
		t.Errorf("fanoutTaken detail should disclose the address-taken abstention, got %q", taken[0].Detail)
	}
}

// O-CX2, the trust-monotonicity invariant as a committed test: across the
// fixture corpus, enabling summaries must never mint a VIOLATED that the
// intraprocedural run did not already report (D-CX2).
func TestLiftMonotonicity(t *testing.T) {
	corpora := []struct {
		name  string
		src   string
		rules []config.ObligationRule
	}{
		{"lift", liftSrc, liftRules()},
		{"release-lift", releaseLiftSrc, []config.ObligationRule{releaseLiftRule()}},
		{"tx", txSrc, []config.ObligationRule{txRule()}},
		{"discharge", dischargeSrc, []config.ObligationRule{
			{Name: "tx-close", Acquire: "example.com/fix#BeginTx", Release: releaseTargets},
			{Name: "guard", Require: "example.com/fix#Commit", Before: "example.com/fix#Rollback", FromCallers: true},
		}},
		{"entry", entrySrc, []config.ObligationRule{
			{Name: "guard", Require: requireRef, Before: "example.com/fix#Publish", FromCallers: true},
		}},
	}
	for _, c := range corpora {
		fns := buildProg(t, c.src)
		var pkgFns []*ssa.Function
		for _, fn := range fns {
			if fn.Pkg != nil && fn.Pkg.Pkg.Path() == "example.com/fix" && fn.Parent() == nil {
				pkgFns = append(pkgFns, fn)
			}
		}
		off := Check(c.rules, pkgFns, "", nil)
		on := Check(c.rules, pkgFns, "", NewSummaries(testUnit(fns)))
		wasViolated := map[string]bool{}
		for _, f := range off {
			if f.Status == Violated {
				wasViolated[f.Rule+"|"+f.Fn+"|"+f.Site] = true
			}
		}
		for _, f := range on {
			if f.Status == Violated && !wasViolated[f.Rule+"|"+f.Fn+"|"+f.Site] {
				t.Errorf("%s: lift minted a new VIOLATED: %+v", c.name, f)
			}
		}
	}
}

// ---- CX-3: ALWAYS-effect summaries ----------------------------------------------

// Derivation is proof-only (O-CX6): a some-paths effect earns nothing.
func TestAlwaysEffect(t *testing.T) {
	src := `package fix

func Send(s string) {}

func emit()        { Send("a") }
func wrap()        { emit() }
func maybe(b bool) {
	if b {
		Send("a")
	}
}
func quiet() {}
`
	fns := buildProg(t, src)
	sites := map[ssa.Instruction]bool{}
	for _, fn := range fns {
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if c, ok := in.(ssa.CallInstruction); ok {
					if sc := c.Common().StaticCallee(); sc != nil && sc.Name() == "Send" {
						sites[in] = true
					}
				}
			}
		}
	}
	s := NewSummaries(testUnit(fns))
	const label = "boundary:test SEND x"
	cases := []struct {
		fn   string
		want bool
	}{
		{"emit", true},
		{"wrap", true}, // composed through the ALWAYS callee
		{"maybe", false},
		{"quiet", false},
	}
	for _, c := range cases {
		if got := s.AlwaysEffect(fnByName(t, fns, c.fn), label, sites); got != c.want {
			t.Errorf("AlwaysEffect(%s) = %v, want %v", c.fn, got, c.want)
		}
	}
}

// ---- CX-1: the must-release handoff credit ---------------------------------------

const releaseLiftSrc = `package fix

type Tx struct{ closed bool }
type Store struct{}

func (s *Store) BeginTx() (*Tx, error) { return &Tx{}, nil }
func (t *Tx) Commit() error            { t.closed = true; return nil }
func (t *Tx) Rollback()                { t.closed = true }

func debit(t *Tx) error { return nil }
func log_()             {}

// The release lives one frame down, on every path of the helper.
func finish(t *Tx, err error) error {
	if err != nil {
		t.Rollback()
		return err
	}
	return t.Commit()
}
func TransferHelper(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	return finish(tx, debit(tx))
}

// The helper releases on one arm only: beyond proof.
func finishMaybe(t *Tx, ok bool) {
	if ok {
		t.Rollback()
	}
}
func TransferHelperLeaky(s *Store, ok bool) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	finishMaybe(tx, ok)
	return nil
}

// A maybe-release followed by an unconditional release still proves: the
// proof hunt walks through the unknown handoff.
func TransferMaybeThenRelease(s *Store, ok bool) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	finishMaybe(tx, ok)
	tx.Rollback()
	return nil
}

// The worked example preserved: debit provably never releases, the leak and
// its witness are unchanged.
func TransferHelperNever(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	return debit(tx)
}

// A non-handoff call earns nothing and blocks nothing.
func TransferPlainLeak(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	_ = tx
	log_()
	return nil
}

// Deferred named ALWAYS-release helper: the deferReleases ceiling, lifted.
func closeTx(t *Tx) { t.Rollback() }
func TransferDeferHelper(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	defer closeTx(tx)
	return debit(tx)
}

// Recursion in the handoff callee abstains.
func recClose(t *Tx, n int) {
	if n > 0 {
		recClose(t, n-1)
		return
	}
	t.Rollback()
}
func TransferRecursive(s *Store) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	recClose(tx, 1)
	return nil
}

// A dynamic handoff is a frontier.
func TransferDynamic(s *Store, f func(*Tx)) error {
	tx, err := s.BeginTx()
	if err != nil {
		return err
	}
	f(tx)
	return nil
}
`

func releaseLiftRule() config.ObligationRule {
	return config.ObligationRule{
		Name:    "tx-must-close",
		Acquire: "example.com/fix#BeginTx",
		Release: []string{"example.com/fix#Commit", "example.com/fix#Rollback"},
	}
}

func TestReleaseLift(t *testing.T) {
	fns := buildProg(t, releaseLiftSrc)
	sums := NewSummaries(testUnit(fns))
	var pkgFns []*ssa.Function
	for _, fn := range fns {
		if fn.Pkg != nil && fn.Pkg.Pkg.Path() == "example.com/fix" && fn.Parent() == nil {
			pkgFns = append(pkgFns, fn)
		}
	}
	fs := Check([]config.ObligationRule{releaseLiftRule()}, pkgFns, "", sums)

	rows := []struct {
		fn   string
		want Status
	}{
		{"TransferHelper", Satisfied},
		{"TransferMaybeThenRelease", Satisfied},
		{"TransferDeferHelper", Satisfied},
		{"TransferHelperLeaky", CantProve},
		{"TransferRecursive", CantProve},
		{"TransferDynamic", CantProve},
		{"TransferHelperNever", Violated},
		{"TransferPlainLeak", Violated},
	}
	for _, r := range rows {
		got := findingsOf(fs, "tx-must-close", r.fn)
		if len(got) != 1 {
			t.Errorf("%s: %d findings, want 1: %v", r.fn, len(got), got)
			continue
		}
		if got[0].Status != r.want {
			t.Errorf("%s = %s (%s), want %s", r.fn, got[0].Status, got[0].Detail, r.want)
		}
	}
	if leaky := findingsOf(fs, "tx-must-close", "TransferHelperLeaky"); len(leaky) == 1 && !strings.Contains(leaky[0].Detail, "finishMaybe") {
		t.Errorf("the abstention must name the unprovable callee, got %q", leaky[0].Detail)
	}
}

// member and sccOf must cover the identical key set after construction — the
// inUnit doc comment's invariant, asserted so the two structures cannot
// silently drift.
func TestUniverseMembershipMatchesCondensation(t *testing.T) {
	fns := buildProg(t, dischargeSrc)
	s := NewSummaries(testUnit(fns))
	if len(s.member) != len(s.sccOf) {
		t.Fatalf("member has %d keys, condensation %d", len(s.member), len(s.sccOf))
	}
	for fn := range s.member {
		if _, ok := s.sccOf[fn]; !ok {
			t.Errorf("%s in member but not in condensation", fn)
		}
	}
}

// Re-binding an effect label with the SAME site set is fine (the production
// adapter passes the same map per label); a DIFFERENT set must fail loudly —
// memoized verdicts were computed against the first binding, and a silent
// stale answer is the exact failure the typed-key change exists to prevent.
func TestAlwaysEffectRebindAsserts(t *testing.T) {
	fns := buildProg(t, dischargeSrc)
	s := NewSummaries(testUnit(fns))
	sites := map[ssa.Instruction]bool{}
	for _, fn := range fns {
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if c, ok := in.(ssa.CallInstruction); ok {
					if sc := c.Common().StaticCallee(); sc != nil && sc.Name() == "Rollback" {
						sites[in] = true
					}
				}
			}
		}
	}
	const label = "boundary:test REBIND"
	_ = s.AlwaysEffect(fnByName(t, fns, "cleanup"), label, sites)

	same := map[ssa.Instruction]bool{}
	for k, v := range sites {
		same[k] = v
	}
	_ = s.AlwaysEffect(fnByName(t, fns, "cleanup"), label, same) // equal set: fine

	defer func() {
		if recover() == nil {
			t.Fatal("re-binding the label with a different site set must panic")
		}
	}()
	_ = s.AlwaysEffect(fnByName(t, fns, "cleanup"), label, map[ssa.Instruction]bool{})
}
