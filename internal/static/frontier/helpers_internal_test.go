package frontier

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/blindspots"
)

// Exhaustiveness guard: every blind-spot kind must have an EXPLICIT bin decision in
// blindSpotBin (recognized==true). When a new blindspots.Kind is added, this fails
// until it is classified — so a kind that is actually reclaimable cannot silently
// fall through the catch-all default into bin A (the fail-open #5 flagged).
func TestBlindSpotBinCoversAllKinds(t *testing.T) {
	for _, k := range blindspots.Kinds() {
		if _, ok := blindSpotBin(string(k)); !ok {
			t.Errorf("blindspots.Kind %q has no explicit frontier bin — add it to blindSpotBin", k)
		}
	}
	// And an unknown kind is reported unrecognized (disclosed as A, but flagged).
	if _, ok := blindSpotBin("SomeFutureKind"); ok {
		t.Error("an unrecognized kind must report recognized==false")
	}
}

// readableDBVerb is the discriminator between a constant-SQL verb the labeler read
// (uppercase: SELECT/DELETE/...) and a method-name fallback it emits for
// non-constant SQL (mixed case: ExecContext, or "call"). Pin both sides.
func TestReadableDBVerb(t *testing.T) {
	readable := []string{"db DELETE provisioning_outbox", "db SELECT users", "db UPDATE loans", "db INSERT", "db MERGE x"}
	opaque := []string{"db ExecContext", "db QueryContext", "db call", "db PingContext", "db "}
	for _, l := range readable {
		if !readableDBVerb(l) {
			t.Errorf("%q should read as a classified verb", l)
		}
	}
	for _, l := range opaque {
		if readableDBVerb(l) {
			t.Errorf("%q should read as opaque (method-name fallback)", l)
		}
	}
}

// closureParent strips a trailing `$N` (a generated closure) and reports the
// lexical parent; a `$`-less name, a non-numeric suffix, or an empty suffix is not
// a closure.
func TestClosureParent(t *testing.T) {
	cases := []struct {
		in     string
		parent string
		ok     bool
	}{
		{"(*pkg.T).Create$1", "(*pkg.T).Create", true},
		{"pkg.Handler$4", "pkg.Handler", true},
		{"(*pkg.T).Outer$2$1", "(*pkg.T).Outer$2", true}, // nested closure: parent is the enclosing closure
		{"(*pkg.T).Create", "", false},                   // not a closure
		{"pkg.Foo$bar", "", false},                       // non-numeric suffix (a `$`-named field, not a closure)
		{"pkg.Foo$", "", false},                          // empty suffix
	}
	for _, c := range cases {
		got, ok := closureParent(c.in)
		if ok != c.ok || got != c.parent {
			t.Errorf("closureParent(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.parent, c.ok)
		}
	}
}
