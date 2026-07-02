// Command taintsvc is the value-flow / taint fixture. It mirrors the PII shape from
// the headroom analysis (a recipient struct with sensitive fields) and exercises
// each trichotomy case with its OWN source/sink functions, so a test can scope the
// analysis to one case and assert FLOW / NO-FLOW / ABSTAIN in isolation. Nothing is
// executed under static analysis.
package main

import (
	"fmt"
	"sort"
)

// Recipient mirrors the real PII carrier. Secret is declared a SOURCE FIELD by the
// field-read test.
type Recipient struct {
	Email  string
	Phone  string
	Secret string
}

// Sources — each returns sensitive data.
func sourceDirect() string { return "pii" }
func sourceRelay() string  { return "pii" }
func sourceReturn() string { return "pii" }
func sourceField() string  { return "pii" }
func sourceMap() string    { return "pii" }
func sourceClean() string  { return "pii" }

// Sinks — each must not receive source taint.
func sinkDirect(string)    {}
func sinkRelay(string)     {}
func sinkReturn(string)    {}
func sinkField(string)     {}
func sinkFieldRead(string) {}
func sinkMap(string)       {}
func sinkClean(string)     {}

// 1. Direct: source result straight into a sink — FLOW.
func caseDirect() { sinkDirect(sourceDirect()) }

// 2. Interprocedural arg→param: source → relay(param) → sink — FLOW.
func relay(x string) { sinkRelay(x) }
func caseRelay()     { relay(sourceRelay()) }

// 3. Interprocedural return-flow: fetch() returns a source; its result → sink — FLOW.
func fetch() string { return sourceReturn() }
func caseReturn()   { sinkReturn(fetch()) }

// 4. Struct-field round trip: store a source into a field, load it, sink — FLOW
// (field-sensitivity, the real PII path shape).
func caseFieldRoundTrip() {
	r := &Recipient{}
	r.Email = sourceField()
	sinkField(r.Email)
}

// 5. Field-read source: Recipient.Secret is a declared SOURCE FIELD — reading it and
// passing it to a sink is a FLOW.
func caseFieldRead(r *Recipient) {
	sinkFieldRead(r.Secret)
}

// 6. Map escape: a source value into a map, read back, then a sink — ABSTAIN. The
// map store is the frontier; the analysis refuses to claim no-flow.
func caseMap() {
	m := map[string]string{}
	m["k"] = sourceMap()
	sinkMap(m["k"])
}

// 7. No-flow: a source is called but its result is dropped; the sink gets a constant
// — NO-FLOW (a sound proof: the source's complete forward cone never reaches a sink).
func caseClean() {
	_ = sourceClean()
	sinkClean("constant")
}

// 8. Slice index: a source returns a slice, indexed and passed to a sink. Indexing a
// tainted slice (*ssa.IndexAddr) is the analysis frontier, so this must be ABSTAIN —
// it is the soundness backstop guard: without the default-escape it would have been a
// false NO-FLOW.
func sourceSlice() []string { return []string{"pii"} }
func sinkSlice(string)      {}
func caseSliceIndex() {
	xs := sourceSlice()
	sinkSlice(xs[0])
}

// 9. Pointer-receiver source: a sensitive field read located ONLY inside a *T method.
// MethodSet(T) omits pointer-receiver methods, so firstPartyFuncs must walk BOTH the
// value and pointer method sets or this read is never indexed/seeded — a false
// NO-FLOW, the verdict this analysis must never emit. Truth = FLOW.
type PtrCarrier struct{ Token string }

func sinkPtr(string) {}

func (p *PtrCarrier) leak() { sinkPtr(p.Token) }

// C-3: taint returned THROUGH an interface (invoke) method. Getter.Get is
// dispatched dynamically; the concrete getter returns a declared source, so the
// tainted return must flow through the invoke result to the sink. The old
// return-flow indexed only STATIC callers, so this was a false NO-FLOW. Truth = FLOW.
type Getter interface{ Get() string }

type piiGetter struct{}

func (piiGetter) Get() string { return sourceIface() }

func sourceIface() string { return "pii" }
func sinkIface(string)    {}

func caseIfaceReturn() {
	var g Getter = piiGetter{}
	sinkIface(g.Get())
}

// C-4: a declared SOURCE method invoked via an interface. Provider.Provide IS the
// declared source; invoked through the interface it must still be seeded — the old
// seed matched sources only at static call sites, so this read Sources==0 → false
// NO-FLOW. Truth = FLOW.
type Provider interface{ Provide() string }

type piiProvider struct{}

func (piiProvider) Provide() string { return "pii" }

func sinkProvide(string) {}

func caseIfaceSource() {
	var p Provider = piiProvider{}
	sinkProvide(p.Provide())
}

// C-5: a struct carrying a tainted field, handed WHOLE to unmodeled code. Storing a
// source into a field taints (type,field); passing the WHOLE struct to an unmodeled
// callee (fmt.Println) must ESCAPE, not read as a proven no-flow. Truth = ABSTAIN.
func sourceCarry() string { return "pii" }

type Carrier struct{ Secret string }

func caseStructCarry() {
	var c Carrier
	c.Secret = sourceCarry()
	fmt.Println(c)
}

// C-3 (arg→param leg): a tainted ARGUMENT passed into an interface method must
// reach the parameter inside the concrete impl. go/ssa's invoke Args exclude the
// receiver while the method's Params[0] IS the receiver, so a naive Args[i]→Params[i]
// taints the receiver and misses the real parameter. Truth = FLOW.
type ArgSink interface{ Consume(s string) }

type argImpl struct{}

func (argImpl) Consume(s string) { sinkIfaceArg(s) }

func sourceIfaceArg() string { return "pii" }
func sinkIfaceArg(string)    {}

func caseIfaceArg() {
	var a ArgSink = argImpl{}
	a.Consume(sourceIfaceArg())
}

// C-3 (func-value return leg): a first-party function reached ONLY as a plain func
// value (not a method, not a static call) that returns a source — its result at the
// call site must carry the taint. Truth = FLOW.
func fetchVal() string      { return sourceFuncVal() }
func sourceFuncVal() string { return "pii" }
func sinkFuncVal(string)    {}

func caseFuncValReturn() {
	var f func() string = fetchVal
	sinkFuncVal(f())
}

// C-4 / C-1 (generic source): a declared source that is a first-party GENERIC
// function. Its concrete callee is an instance with a nil ssa.Pkg and a type-arg
// name, so a fn.Name()+PkgPath matcher never seeds it. Truth = FLOW.
func GenericSource[T any](t T) string { return "pii" }

func sinkGeneric(string) {}

func caseGenericSource() {
	sinkGeneric(GenericSource[int](0))
}

// R-4: a first-party method whose tainted RETURN is consumed by a THIRD-PARTY
// caller. SortLeak implements sort.Interface; sort.Sort calls Less from inside the
// sort package — a foreign frame the analysis cannot descend into — and Less
// returns a source-tainted value. Return-flow taints only first-party callers, so
// without the foreign-caller escape Less's taint would propagate nowhere and set no
// escape → a false NO-FLOW. (Boxing the untainted SortLeak into sort.Interface does
// NOT escape, so the return-flow is the ONLY escape channel here.) Truth = ABSTAIN.
type SortLeak []int

func sourceSort() string { return "pii" }

func (s SortLeak) Len() int           { return len(s) }
func (s SortLeak) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s SortLeak) Less(i, j int) bool { return sourceSort() < "z" }

func caseSortLeak() {
	sort.Sort(SortLeak{3, 1, 2})
}

func main() {
	caseSortLeak()
	caseDirect()
	caseRelay()
	caseReturn()
	caseFieldRoundTrip()
	caseFieldRead(&Recipient{})
	caseMap()
	caseClean()
	caseSliceIndex()
	(&PtrCarrier{}).leak()
	caseIfaceReturn()
	caseIfaceSource()
	caseStructCarry()
	caseIfaceArg()
	caseFuncValReturn()
	caseGenericSource()
}
