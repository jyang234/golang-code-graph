// Command taintsvc is the value-flow / taint fixture. It mirrors the PII shape from
// the headroom analysis (a recipient struct with sensitive fields) and exercises
// each trichotomy case with its OWN source/sink functions, so a test can scope the
// analysis to one case and assert FLOW / NO-FLOW / ABSTAIN in isolation. Nothing is
// executed under static analysis.
package main

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

func main() {
	caseDirect()
	caseRelay()
	caseReturn()
	caseFieldRoundTrip()
	caseFieldRead(&Recipient{})
	caseMap()
	caseClean()
	caseSliceIndex()
}
