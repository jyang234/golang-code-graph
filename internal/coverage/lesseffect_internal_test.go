package coverage

import "testing"

// TestLessEffectTotalOrder pins that the Delta sort comparator is a TOTAL order:
// two effects that tie on Tier and Key but differ in Category must still have a
// deterministic order (broken by Category), not fall back to append order. Per
// CLAUDE.md, every tie-break resolves on intrinsic data.
func TestLessEffectTotalOrder(t *testing.T) {
	a := Effect{Category: Consume, Key: "X", Tier: 1}
	b := Effect{Category: Publish, Key: "X", Tier: 1} // same Tier+Key, different Category
	ab, ba := lessEffect(a, b), lessEffect(b, a)
	if ab == ba {
		t.Fatalf("Tier+Key tie not broken: lessEffect(a,b)=%v lessEffect(b,a)=%v (not a total order)", ab, ba)
	}
	// Category is the intrinsic tie-break: "consume" < "publish".
	if !ab {
		t.Errorf("tie should order by Category (consume < publish), got b before a")
	}
}
