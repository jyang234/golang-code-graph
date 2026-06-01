// Package irtest provides shared builders for constructing ir.CanonicalTrace
// values in the engine's tests, so the IR's construction shape lives in one
// place rather than being re-spelled in every package's _test file.
package irtest

import "github.com/jyang234/golang-code-graph/ir"

// Span builds a CanonicalSpan with tier 1 (the common default; set s.Tier after
// for a specific tier).
func Span(op string, kind ir.Kind, peer string, kids ...ir.ChildGroup) *ir.CanonicalSpan {
	return &ir.CanonicalSpan{Op: op, Kind: kind, Peer: peer, Tier: 1, Children: kids}
}

// Seq builds a sequential (happens-before) child group.
func Seq(members ...*ir.CanonicalSpan) ir.ChildGroup {
	return ir.ChildGroup{Members: members}
}

// Conc builds a concurrent (race-ordered) child group.
func Conc(members ...*ir.CanonicalSpan) ir.ChildGroup {
	return ir.ChildGroup{Concurrent: true, Members: members}
}

// Trace wraps a root span in a CanonicalTrace with a fixed flow id.
func Trace(service string, root *ir.CanonicalSpan) *ir.CanonicalTrace {
	return &ir.CanonicalTrace{Flow: "f", Service: service, Root: root}
}
