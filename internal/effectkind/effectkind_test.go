package effectkind

import (
	"sort"
	"testing"
)

func TestMethodNamedSetIsSortedAndConsistent(t *testing.T) {
	got := MethodNamedKinds()
	if !sort.StringsAreSorted(got) {
		t.Errorf("MethodNamedKinds() = %v, want sorted (determinism)", got)
	}
	for _, k := range got {
		if !IsMethodNamed(k) {
			t.Errorf("IsMethodNamed(%q) = false, want true (set/predicate disagree)", k)
		}
	}
	if IsMethodNamed("http") || IsMethodNamed("db") || IsMethodNamed("bus") || IsMethodNamed("") {
		t.Error("IsMethodNamed must be false for non-method-named kinds")
	}
	// MethodNamedKinds returns a fresh copy — mutating it must not affect the source.
	got[0] = "mutated"
	if IsMethodNamed("mutated") {
		t.Error("MethodNamedKinds must return a copy, not alias the backing set")
	}
}
