package impeach

import (
	"testing"

	"github.com/jyang234/golang-code-graph/internal/canon/opkey"
	"github.com/jyang234/golang-code-graph/internal/groundwork/graph"
	"github.com/jyang234/golang-code-graph/internal/sqlverb"
)

// staticDBKey decodes a "boundary:db <OP> <table>" label through the REAL static
// decoder (graph.DBEffects, the schema owner) and renders the canonical join key.
// It deliberately does not re-parse the label here — that would defeat the parity
// the test exists to prove.
func staticDBKey(t *testing.T, label string) (key string, ok bool) {
	t.Helper()
	ix := graph.NewIndex(&graph.Graph{
		Edges: []graph.Edge{{From: "svc.emit", To: label, Boundary: "outbound-sync"}},
	})
	effs, _ := ix.DBEffects()
	if len(effs) != 1 {
		return "", false
	}
	return DBEffectKey(effs[0].Op, effs[0].Table), true
}

// observedDBKey decodes a "DB <system> <OP> <table>" op key through the REAL
// behavioral decoder (opkey.ParseDBKey) and renders the canonical join key.
func observedDBKey(t *testing.T, op string) (key string, ok bool) {
	t.Helper()
	_, operation, table, parsed := opkey.ParseDBKey(op)
	if !parsed || operation == "" {
		return "", false
	}
	return DBEffectKey(operation, table), true
}

// TestDBEffectKeyParity is the one-source guard (CLAUDE.md): the static label and
// the behavioral op key for the SAME write must reduce to the SAME canonical join
// key, across the full write vocabulary plus a read. If either spelling convention
// shifts (graphio's "boundary:db" grammar or opkey's "DB <system>" grammar), this
// breaks before the impeachment join can silently mis-key — missing real
// impeachments or fabricating spurious ones.
func TestDBEffectKeyParity(t *testing.T) {
	// Reads reconcile too (a false NEVER on a SELECT is also analyzer
	// unsoundness); the write set is exercised explicitly because it is the
	// marquee impeachment target (plan §14-A) and the verbs are the one-source
	// sqlverb vocabulary — derived, never hand-typed.
	verbs := append(sqlverb.MutatingVerbs(), "SELECT")
	for _, verb := range verbs {
		t.Run(verb, func(t *testing.T) {
			want := "db " + verb + " ledger"

			sk, ok := staticDBKey(t, "boundary:db "+verb+" ledger")
			if !ok {
				t.Fatalf("static decoder produced no effect for %q", verb)
			}
			if sk != want {
				t.Errorf("static key = %q, want %q", sk, want)
			}

			ok2 := false
			var bk string
			bk, ok2 = observedDBKey(t, "DB postgresql "+verb+" ledger")
			if !ok2 {
				t.Fatalf("behavioral decoder produced no effect for %q", verb)
			}
			if bk != want {
				t.Errorf("behavioral key = %q, want %q", bk, want)
			}

			if sk != bk {
				t.Errorf("parity broken: static %q != behavioral %q", sk, bk)
			}
		})
	}
}

// TestDBEffectKeySystemAgnostic pins the soundness-forced decision (§14-A): the
// behavioral DB system is dropped from the join key, so two systems writing the
// same table key identically — and both match the system-less static negative.
// Were the system retained, no DB write could ever match a static negative.
func TestDBEffectKeySystemAgnostic(t *testing.T) {
	pg, ok1 := observedDBKey(t, "DB postgresql DELETE ledger")
	my, ok2 := observedDBKey(t, "DB mysql DELETE ledger")
	if !ok1 || !ok2 {
		t.Fatal("decoder rejected a well-formed DB op key")
	}
	if pg != my {
		t.Errorf("system leaked into key: %q != %q", pg, my)
	}
	if pg != "db DELETE ledger" {
		t.Errorf("key = %q, want %q", pg, "db DELETE ledger")
	}
}

// TestDBEffectOpaqueNotKeyed pins that an unreadable effect on either side yields
// NO key rather than a fabricated one — fail closed, never guess a table. An
// opaque static label ("boundary:db Exec") is tallied as unreadable; an op-only
// behavioral key ("DB postgresql") has no operation to assert.
func TestDBEffectOpaqueNotKeyed(t *testing.T) {
	if _, ok := staticDBKey(t, "boundary:db Exec"); ok {
		t.Error("opaque static label was keyed; want unreadable")
	}
	ix := graph.NewIndex(&graph.Graph{
		Edges: []graph.Edge{{From: "svc.emit", To: "boundary:db Exec", Boundary: "outbound-sync"}},
	})
	if effs, unreadable := ix.DBEffects(); len(effs) != 0 || unreadable != 1 {
		t.Errorf("DBEffects(boundary:db Exec) = %d effects, %d unreadable; want 0, 1", len(effs), unreadable)
	}
	if _, ok := observedDBKey(t, "DB postgresql"); ok {
		t.Error("system-only behavioral key was keyed; want unreadable")
	}
}
