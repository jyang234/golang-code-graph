package schemadrift

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/jyang234/golang-code-graph/internal/canon/sql"
)

// edge is a small helper for building a DB boundary edge from an owner + label.
func dbEdge(owner, label string) Edge { return Edge{From: owner, To: dbBoundaryPrefix + label} }

func mig(name, sqlText string) MigrationFile { return MigrationFile{Name: name, SQL: sqlText} }

// driftTables returns the sorted drifted table names for a concise assertion.
func driftTables(r Report) []string {
	out := make([]string, 0, len(r.Drift))
	for _, d := range r.Drift {
		out = append(out, d.Table)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCleanService: a code write to a migration-defined table is no drift.
func TestCleanService(t *testing.T) {
	edges := []Edge{dbEdge("svc.Insert", "INSERT event_types")}
	files := []MigrationFile{mig("V1__init.sql", "CREATE TABLE event_types (id text);")}
	r := Check(edges, files, nil)
	if len(r.Drift) != 0 {
		t.Fatalf("clean service flagged drift: %v", driftTables(r))
	}
	if r.Migrations != 1 {
		t.Fatalf("migrations replayed = %d, want 1", r.Migrations)
	}
}

// TestOutboxFalsePositive pins the load-bearing completeness condition: a
// library-owned table (no Flyway script creates it) false-fires as drift UNTIL it
// is declared library-owned. This is the provisioning_outbox class the prototype
// caught.
func TestOutboxFalsePositive(t *testing.T) {
	edges := []Edge{
		dbEdge("svc.Insert", "INSERT event_types"),
		dbEdge("outbox.Publish", "INSERT provisioning_outbox"),
	}
	files := []MigrationFile{mig("V1__init.sql", "CREATE TABLE event_types (id text);")}

	naive := Check(edges, files, nil)
	if !eq(driftTables(naive), []string{"provisioning_outbox"}) {
		t.Fatalf("without library-owned, want drift [provisioning_outbox], got %v", driftTables(naive))
	}

	folded := Check(edges, files, []string{"provisioning_outbox"})
	if len(folded.Drift) != 0 {
		t.Fatalf("with provisioning_outbox declared library-owned, want clean, got %v", driftTables(folded))
	}
	if !eq(folded.LibraryOwned, []string{"provisioning_outbox"}) {
		t.Fatalf("LibraryOwned = %v, want [provisioning_outbox]", folded.LibraryOwned)
	}
}

// TestDropReplay: a table created then dropped across versioned migrations drops out
// of the schema set (so a still-present write to it is drift), and a *_rollback.sql
// is excluded from the forward replay.
func TestDropReplay(t *testing.T) {
	files := []MigrationFile{
		mig("V3__queues.sql", "CREATE TABLE queue_messages (id text); CREATE TABLE event_types (id text);"),
		mig("V8__drop_queues.sql", "DROP TABLE queue_messages;"),
		// A rollback that would re-create the table if (wrongly) replayed forward.
		mig("V8__drop_queues_rollback.sql", "CREATE TABLE queue_messages (id text);"),
	}
	edges := []Edge{
		dbEdge("svc.A", "INSERT event_types"),
		dbEdge("svc.B", "INSERT queue_messages"),
	}
	r := Check(edges, files, nil)
	if !eq(driftTables(r), []string{"queue_messages"}) {
		t.Fatalf("want queue_messages drifted (dropped, rollback excluded), got %v", driftTables(r))
	}
	if r.Migrations != 2 {
		t.Fatalf("migrations replayed = %d, want 2 (rollback excluded)", r.Migrations)
	}
}

// TestDriftFires: a genuinely undefined table appears in Drift with its ops/sites.
func TestDriftFires(t *testing.T) {
	edges := []Edge{
		dbEdge("svc.Write", "INSERT ghost_table"),
		dbEdge("svc.Update", "UPDATE ghost_table"),
	}
	r := Check(edges, nil, nil)
	if len(r.Drift) != 1 || r.Drift[0].Table != "ghost_table" {
		t.Fatalf("want one drift on ghost_table, got %v", driftTables(r))
	}
	if !eq(r.Drift[0].Ops, []string{"INSERT", "UPDATE"}) {
		t.Fatalf("ops = %v, want [INSERT UPDATE]", r.Drift[0].Ops)
	}
	if !eq(r.Drift[0].Sites, []string{"svc.Update", "svc.Write"}) {
		t.Fatalf("sites = %v, want [svc.Update svc.Write]", r.Drift[0].Sites)
	}
}

// TestOpaqueWrites: unresolved DB writes (driver-method fallback, bare-verb fold)
// are counted as opaque, never as drift — the db-call frontier.
func TestOpaqueWrites(t *testing.T) {
	edges := []Edge{
		dbEdge("svc.A", "ExecContext"),    // driver method fallback
		dbEdge("svc.B", "call"),           // indirect callee
		dbEdge("svc.C", "DELETE"),         // bare-verb fold, table unknown
		dbEdge("svc.D", "INSERT defined"), // resolved
	}
	files := []MigrationFile{mig("V1__init.sql", "CREATE TABLE defined (id text);")}
	r := Check(edges, files, nil)
	if r.OpaqueWrites != 3 {
		t.Fatalf("OpaqueWrites = %d, want 3", r.OpaqueWrites)
	}
	if len(r.Drift) != 0 {
		t.Fatalf("opaque writes must not drift, got %v", driftTables(r))
	}
}

// TestAdvisory: a defined-but-untouched table is advisory only; a library-owned
// untouched table is not even advisory (we know the library owns it).
func TestAdvisory(t *testing.T) {
	files := []MigrationFile{mig("V1__init.sql", "CREATE TABLE touched (id text); CREATE TABLE lonely (id text);")}
	edges := []Edge{dbEdge("svc.A", "SELECT touched")}
	r := Check(edges, files, []string{"library_only"})
	if !eq(r.Advisory, []string{"lonely"}) {
		t.Fatalf("advisory = %v, want [lonely]", r.Advisory)
	}
}

// TestRenameCaveat: a table rename is surfaced as a fail-closed caveat.
func TestRenameCaveat(t *testing.T) {
	files := []MigrationFile{mig("V2__rename.sql", "ALTER TABLE old_name RENAME TO new_name;")}
	r := Check(nil, files, nil)
	if len(r.ParseCaveats) != 1 {
		t.Fatalf("want 1 rename caveat, got %v", r.ParseCaveats)
	}
}

// TestDeterminism: the report is byte-identical regardless of the order edges and
// migration files are supplied in (CLAUDE.md obligation for any new ordering path).
func TestDeterminism(t *testing.T) {
	edges := []Edge{
		dbEdge("svc.A", "INSERT event_types"),
		dbEdge("svc.B", "INSERT ghost"),
		dbEdge("svc.C", "ExecContext"),
		dbEdge("svc.D", "SELECT event_types"),
	}
	files := []MigrationFile{
		mig("V1__a.sql", "CREATE TABLE event_types (id text); CREATE TABLE tmp (id text);"),
		mig("V2__b.sql", "DROP TABLE tmp;"),
		mig("R__view.sql", "CREATE TABLE rep (id text);"),
		mig("V1__a_rollback.sql", "DROP TABLE event_types;"),
	}
	want, err := json.Marshal(Check(edges, files, []string{"lib_owned"}))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		e := append([]Edge(nil), edges...)
		rng.Shuffle(len(e), func(a, b int) { e[a], e[b] = e[b], e[a] })
		f := append([]MigrationFile(nil), files...)
		rng.Shuffle(len(f), func(a, b int) { f[a], f[b] = f[b], f[a] })
		got, err := json.Marshal(Check(e, f, []string{"lib_owned"}))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("schema-drift report depends on input order:\n want %s\n got  %s", want, got)
		}
	}
}

// TestVerbParity pins tableNamingVerb against canon/sql: every verb in the set is a
// verb canon/sql names a table for, and a tx-control verb (not in the set) is not.
// This is the one-source-of-truth guard — the check parses the label the same way
// graphio (via canon/sql) produced it.
func TestVerbParity(t *testing.T) {
	samples := map[string]string{
		"SELECT":  "SELECT * FROM t",
		"INSERT":  "INSERT INTO t VALUES (1)",
		"UPDATE":  "UPDATE t SET x = 1",
		"DELETE":  "DELETE FROM t",
		"MERGE":   "MERGE INTO t USING s ON t.id = s.id",
		"REPLACE": "REPLACE INTO t VALUES (1)",
		"UPSERT":  "UPSERT INTO t VALUES (1)",
	}
	for verb := range tableNamingVerb {
		stmt, ok := samples[verb]
		if !ok {
			t.Fatalf("tableNamingVerb has %q with no canon/sql parity sample — add one", verb)
		}
		n := sql.Normalize(stmt)
		if n.Operation != verb {
			t.Errorf("canon/sql op for %q = %q, want %q (label format drift)", stmt, n.Operation, verb)
		}
		if n.Table == "" {
			t.Errorf("canon/sql named no table for %q — %q would be parsed as opaque", stmt, verb)
		}
	}
	// A tx-control statement must NOT be a table-naming verb, so it cannot masquerade
	// as a resolved write.
	if tableNamingVerb[sql.Normalize("BEGIN").Operation] {
		t.Errorf("BEGIN classified as a table-naming verb")
	}
}

// TestCodeTablesLabelFormat pins the label-parse contract: a resolved write reads
// back as "<VERB> <table>"; method-name and bare-verb forms are opaque.
func TestCodeTablesLabelFormat(t *testing.T) {
	writes, opaque := codeTables([]Edge{
		dbEdge("o1", "INSERT users"),
		dbEdge("o2", "ExecContext"),
		dbEdge("o3", "DELETE"),
		{From: "o4", To: "internal.fn"}, // not a DB boundary edge
	})
	if _, ok := writes["users"]; !ok || len(writes) != 1 {
		t.Fatalf("want one resolved table 'users', got %v", sortedKeys(writes))
	}
	if opaque != 2 {
		t.Fatalf("opaque = %d, want 2 (ExecContext, bare DELETE)", opaque)
	}
}
