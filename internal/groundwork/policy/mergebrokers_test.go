package policy

import (
	"reflect"
	"testing"
)

// M-4: two policies declaring the SAME broker name with DIFFERENT guarantees have
// no single source — MergeBrokers must report the conflict, and the conflict names
// must be SORTED so the caller's refusal is byte-identical run to run (the CLI used
// to range a map and print whichever name it visited first). An identical
// re-declaration is harmless.
func TestMergeBrokersConflictsSortedAndDeterministic(t *testing.T) {
	a := map[string]Broker{
		"zbus": {Delivery: "at-least-once"},
		"abus": {Delivery: "exactly-once"},
		"same": {Delivery: "at-most-once"},
	}
	b := map[string]Broker{
		"zbus": {Delivery: "at-most-once"},  // conflicts with a["zbus"]
		"abus": {Delivery: "at-least-once"}, // conflicts with a["abus"]
		"same": {Delivery: "at-most-once"},  // identical — not a conflict
	}

	// Run several times; the conflict list must be identical and sorted every time,
	// regardless of the two maps' iteration order.
	var first []string
	for i := 0; i < 20; i++ {
		_, conflicts := MergeBrokers([]map[string]Broker{a, b})
		want := []string{"abus", "zbus"}
		if !reflect.DeepEqual(conflicts, want) {
			t.Fatalf("conflicts = %v, want sorted %v", conflicts, want)
		}
		if first == nil {
			first = conflicts
		} else if !reflect.DeepEqual(conflicts, first) {
			t.Fatalf("conflict list not deterministic: %v vs %v", conflicts, first)
		}
	}
}

// No conflict → empty list and a fully merged map (identical re-declarations fold).
func TestMergeBrokersNoConflict(t *testing.T) {
	a := map[string]Broker{"bus": {Delivery: "exactly-once"}}
	b := map[string]Broker{"bus": {Delivery: "exactly-once"}, "other": {Delivery: "at-least-once"}}
	merged, conflicts := MergeBrokers([]map[string]Broker{a, b})
	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts %v", conflicts)
	}
	if len(merged) != 2 || merged["bus"].Delivery != "exactly-once" || merged["other"].Delivery != "at-least-once" {
		t.Errorf("merged map wrong: %#v", merged)
	}
}
