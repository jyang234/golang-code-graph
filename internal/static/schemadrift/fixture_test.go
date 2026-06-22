package schemadrift

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/static/analyze"
	"github.com/jyang234/golang-code-graph/internal/static/callgraph"
	"github.com/jyang234/golang-code-graph/internal/static/graphio"
)

// TestSchemaDriftSvcFixture is the end-to-end proof on a real service: it runs the
// actual analyzer + graphio.Build on the schemadriftsvc fixture, adapts the emitted
// graph edges, replays the fixture's migrations, and checks the report. This is the
// only test that exercises the producer→label→parse path on a genuinely
// graphio-emitted graph (the unit table uses synthetic labels), so it pins that the
// check parses the label format the builder actually produces.
func TestSchemaDriftSvcFixture(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "fixtures", "schemadriftsvc")

	res, err := analyze.Analyze(root, callgraph.Options{Algo: callgraph.AlgoVTA})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	g, err := graphio.Build(res, "")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	edges := make([]Edge, len(g.Edges))
	for i, e := range g.Edges {
		edges[i] = Edge{From: e.From, To: e.To}
	}
	files, err := LoadMigrations(filepath.Join(root, "db", "migrations"))
	if err != nil {
		t.Fatalf("migrations: %v", err)
	}

	r := Check(edges, files, []string{"provisioning_outbox"})

	// Drift = the genuinely-undefined table (audit_log) + the dropped one
	// (queue_messages). event_types is defined and provisioning_outbox is
	// library-owned, so both read clean.
	if got := driftTables(r); !eq(got, []string{"audit_log", "queue_messages"}) {
		t.Errorf("drift = %v, want [audit_log queue_messages]", got)
	}
	// archived_events is defined but never written — advisory, not drift.
	if !eq(r.Advisory, []string{"archived_events"}) {
		t.Errorf("advisory = %v, want [archived_events]", r.Advisory)
	}
	// PurgeStale's non-constant SQL is the db-call frontier: one opaque write.
	if r.OpaqueWrites != 1 {
		t.Errorf("opaque writes = %d, want 1 (PurgeStale)", r.OpaqueWrites)
	}
	// V1 + V2 replayed; the *_rollback.sql excluded.
	if r.Migrations != 2 {
		t.Errorf("migrations replayed = %d, want 2 (rollback excluded)", r.Migrations)
	}

	// Completeness condition on real code: without the library-owned declaration the
	// outbox false-fires as drift (the provisioning_outbox class the prototype caught).
	naive := Check(edges, files, nil)
	if !contains(driftTables(naive), "provisioning_outbox") {
		t.Errorf("without library-owned, provisioning_outbox must false-fire; drift = %v", driftTables(naive))
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
