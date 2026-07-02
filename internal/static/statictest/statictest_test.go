package statictest

import (
	"strings"
	"testing"
)

// TestFindFuncPanicsOnAmbiguity pins M-22: a substring matching two unrelated
// functions (here the generic origin codec.Decode and its instantiation
// codec.Decode[…]) must fail loudly rather than return a map-iteration-order
// pick. FindFuncExact remains the escape hatch.
func TestFindFuncPanicsOnAmbiguity(t *testing.T) {
	prog, err := Build()
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected FindFunc to panic on ambiguous substring")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "ambiguous") {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()
	FindFunc(prog, "internal/codec.Decode") // origin + instantiation both match
	t.Fatal("FindFunc did not panic")
}

// TestFindFuncClosureNotAmbiguous pins the documented parent/closure contract:
// a substring that matches a parent and its closures is NOT ambiguous — the
// parent is returned.
func TestFindFuncClosureNotAmbiguous(t *testing.T) {
	prog, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	// loansvc.run has closures (loansvc.run$1, …); the bare name must still
	// resolve to the parent without panicking.
	fn := FindFunc(prog, "loansvc.run")
	if fn == nil {
		t.Fatal("loansvc.run not found")
	}
	if got := fn.RelString(nil); strings.Contains(got, "$") {
		t.Fatalf("FindFunc returned a closure %q, want the parent", got)
	}
}
