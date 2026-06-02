package ingest

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jyang234/golang-code-graph/ir"
)

// TestCompareEffectsNoNewEffects pins the D-PH3 semantics: only effects observed
// but absent from the golden are "added" (gate-failing); golden effects not
// observed are "missing" (informational, never a failure).
func TestCompareEffectsNoNewEffects(t *testing.T) {
	golden := []string{"PUBLISH a", "HTTP GET peer /x"}
	observed := []string{"HTTP GET peer /x", "PUBLISH b"} // dropped a, added b

	added, missing := CompareEffects(golden, observed)
	if !reflect.DeepEqual(added, []string{"PUBLISH b"}) {
		t.Errorf("added = %v, want [PUBLISH b]", added)
	}
	if !reflect.DeepEqual(missing, []string{"PUBLISH a"}) {
		t.Errorf("missing = %v, want [PUBLISH a]", missing)
	}
}

func TestCompareEffectsClean(t *testing.T) {
	set := []string{"PUBLISH a", "CONSUME b"}
	if added, _ := CompareEffects(set, set); len(added) != 0 {
		t.Errorf("identical sets should add nothing, got %v", added)
	}
	// A superset golden (flow under-exercised this run) adds nothing → gate passes.
	if added, missing := CompareEffects([]string{"PUBLISH a", "PUBLISH b"}, []string{"PUBLISH a"}); len(added) != 0 || len(missing) != 1 {
		t.Errorf("under-exercise should be missing-only, got added=%v missing=%v", added, missing)
	}
}

func TestBoundaryEffectsFiltersNonBoundary(t *testing.T) {
	root := &ir.CanonicalSpan{
		Op: "HTTP POST /loan-application", Kind: ir.KindServer,
		Children: []ir.ChildGroup{{Members: []*ir.CanonicalSpan{
			{Op: "PUBLISH loan.approved", Kind: ir.KindProducer},
			{Op: "DB postgres INSERT ledger", Kind: ir.KindClient}, // not a boundary effect
			{Op: "computeScore", Kind: ir.KindInternal},            // internal, dropped
			{Op: "HTTP GET credit-bureau /score/{id}", Kind: ir.KindClient},
		}}},
	}
	got := BoundaryEffects(root)
	want := []string{
		"HTTP GET credit-bureau /score/{id}",
		"HTTP POST /loan-application",
		"PUBLISH loan.approved",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BoundaryEffects = %v, want %v (DB + internal excluded, sorted)", got, want)
	}
}

func TestEffectGoldenRoundTrip(t *testing.T) {
	g := NewEffectGolden("loan", "loansvc", []string{"PUBLISH b", "PUBLISH a", "PUBLISH a"}) // unsorted + dup
	if !reflect.DeepEqual(g.Effects, []string{"PUBLISH a", "PUBLISH b"}) {
		t.Errorf("NewEffectGolden should sort and dedup, got %v", g.Effects)
	}
	b, err := g.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if b[len(b)-1] != '\n' {
		t.Error("marshaled golden should be newline-terminated")
	}
	// Marshal is deterministic.
	b2, _ := g.Marshal()
	if string(b) != string(b2) {
		t.Error("marshal is not deterministic")
	}
}

// TestLoadEffectGoldenRejectsWrongSchema: a golden with a foreign/stale schema
// version is a hard error, not a silent accept (D-PH finding #8).
func TestLoadEffectGoldenRejectsWrongSchema(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/x.effects.json"
	if err := os.WriteFile(p, []byte(`{"schema_version":"flowmap.trace/v1","flow":"x","service":"s","effects":["PUBLISH a"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEffectGolden(p); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected a schema-mismatch error, got %v", err)
	}
}

// TestEffectGoldenMarshalNoHTMLEscape: an op key with '&'/'<'/'>' survives
// verbatim through the canonical serializer rather than being HTML-escaped
// (finding #7 — gated artifacts go through canonjson).
func TestEffectGoldenMarshalNoHTMLEscape(t *testing.T) {
	g := NewEffectGolden("f", "s", []string{"HTTP GET peer /a?b=1&c=2"})
	b, err := g.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "&c=2") {
		t.Errorf("op key was HTML-escaped (want verbatim '&'):\n%s", b)
	}
}
