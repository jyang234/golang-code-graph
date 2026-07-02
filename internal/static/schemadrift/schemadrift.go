// Package schemadrift is the deterministic schema-drift cross-check: it diffs the
// DB tables the code WRITES (already named in the emitted graph as
// "boundary:db <OP> <table>" labels) against the tables the migrations DEFINE. A
// code-side table absent from the defined schema is the `relation "X" does not
// exist` deploy-hazard class.
//
// It is a pure post-process — it never touches the graph build, so it carries zero
// risk to the analyzer's determinism or soundness. The core (Check) is decoupled
// from the graph builder: it consumes a minimal Edge view and a list of migration
// files, mirroring how frontier.Input decouples from graphio.
//
// Soundness (earned by the prototype on event-bus/cgate; see
// docs/design/schema-drift-check-plan.md): a drift flag is sound — i.e. every flag
// is a real hazard — ONLY IF the defined-schema set is COMPLETE (every real table
// present). The set is therefore Flyway-replay ∪ caller-declared library-owned
// tables (the outbox/inbox pattern is auto-migrated by a library and named by no
// Flyway script — folding it in is the load-bearing completeness condition). The
// check also INHERITS the db-call frontier: a write whose SQL is non-constant
// carries no table, so it is counted as an opaque write, never a resolved one.
// "No drift" therefore means "no drift among resolved writes" — pair it with the
// disclosed OpaqueWrites count, exactly as the budget/ratchet pairs counts with the
// db-call disclosure.
//
// Determinism: every output collection is sorted on an intrinsic key (table name);
// migration replay is version-ordered; no map-iteration order reaches the output.
package schemadrift

import (
	"sort"
	"strings"

	"github.com/jyang234/golang-code-graph/internal/boundarylabel"
)

// dbBoundaryPrefix is the prefix graphio puts on a DB boundary edge's target —
// the shared boundarylabel.DBPrefix — followed by joinOpTable(op, table)
// (internal/static/graphio: labels.go joinOpTable, graphio.go edge construction).
// A resolved write reads back as "boundary:db INSERT event_types"; an unreadable
// statement falls back to the driver method name ("boundary:db ExecContext",
// "boundary:db call") or a table-less bare verb ("boundary:db DELETE"). This
// constant and tableNamingVerb encode that format contract; TestVerbParity /
// TestCodeTables pin it so a change to the emitted label cannot silently break the
// check.
const dbBoundaryPrefix = boundarylabel.DBPrefix

// tableNamingVerb is the set of SQL verbs that name a primary table — the exact
// verbs canon/sql.operationAndTable returns a non-empty Table for. The emitted
// label uses these in upper case, so a label whose first field is in this set AND
// is followed by a table token is a RESOLVED write; anything else (a mixed-case
// driver method name, or a bare verb with no table) is opaque. Kept in parity with
// canon/sql by TestVerbParity.
var tableNamingVerb = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"MERGE": true, "REPLACE": true, "UPSERT": true,
}

// Edge is the minimal view of a graph edge the check consumes: a call from a
// first-party function (From) to a node (To). The CLI adapter copies
// graphio.Edge.{From,To} into this so the core stays decoupled from the graph
// builder (mirrors frontier.Input).
type Edge struct {
	From string
	To   string
}

// MigrationFile is one migration's base name and SQL text. Check filters rollbacks
// and orders by version itself, so the caller may pass files in any order.
type MigrationFile struct {
	Name string
	SQL  string
}

// Report is the cross-check result. Drift is the sound assertion; Advisory and
// OpaqueWrites are disclosures (lower-bound / blind-spot), never gate signals.
type Report struct {
	// Drift: code writes a table the defined schema does not contain — the
	// `relation does not exist` hazard. Sound iff the schema set is complete.
	Drift []DriftItem `json:"drift"`
	// Advisory: schema defines a table the code never resolves a write to. Noisy by
	// construction (a table touched only via opaque SQL has no label), so advisory
	// only — never a gate signal.
	Advisory []string `json:"advisory,omitempty"`
	// OpaqueWrites is the db-call frontier residual: DB boundary edges whose table
	// could not be named (non-constant SQL). The disclosure that scopes "no drift"
	// to resolved writes.
	OpaqueWrites int `json:"opaque_writes"`
	// Migrations is the count of forward migration files replayed (rollbacks excluded).
	Migrations int `json:"migrations"`
	// LibraryOwned echoes the declared library-owned tables folded into the schema set.
	LibraryOwned []string `json:"library_owned,omitempty"`
	// ParseCaveats are fail-closed disclosures from the DDL scan (e.g. a table
	// RENAME the create/drop scan cannot follow), so an incomplete scan is surfaced
	// rather than silently producing drift.
	ParseCaveats []string `json:"parse_caveats,omitempty"`
}

// DriftItem is one drifted table with the operations and owning functions that
// reference it. Ops and Sites are sorted for determinism.
type DriftItem struct {
	Table string   `json:"table"`
	Ops   []string `json:"ops"`
	Sites []string `json:"sites"`
}

// tableWrites accumulates the operations and owners that reference one table.
type tableWrites struct {
	ops   map[string]bool
	sites map[string]bool
}

// Check is the pure cross-check: code DB-write labels (edges) vs the
// migration-defined schema (files) ∪ libraryOwned. It is a pure function of its
// inputs and independent of the order edges/files are supplied in.
func Check(edges []Edge, files []MigrationFile, libraryOwned []string) Report {
	writes, opaque := codeTables(edges)

	defined, replayed, caveats := schemaTables(files)

	// defined := Flyway-replay ∪ library-owned. The union is the completeness
	// condition the prototype proved load-bearing: the outbox/inbox tables are
	// auto-migrated by a library and named by no Flyway script, so without this they
	// false-fire as drift (the provisioning_outbox class).
	libOwned := normalizeSet(libraryOwned)
	for t := range libOwned {
		defined[t] = true
	}

	// Drift (SOUND): a resolved code write whose table the schema does not define.
	drift := make([]DriftItem, 0)
	for _, t := range sortedKeys(writes) {
		if defined[t] {
			continue
		}
		w := writes[t]
		drift = append(drift, DriftItem{
			Table: t,
			Ops:   sortedKeys(w.ops),
			Sites: sortedKeys(w.sites),
		})
	}

	// Advisory (NOISY, lower-bound): a defined table the code never resolves a write
	// to. Library-owned tables are excluded — we already know the library owns them,
	// so their absence from the code's resolved-write set is not signal.
	var advisory []string
	for _, t := range sortedKeys(defined) {
		if _, touched := writes[t]; touched {
			continue
		}
		if libOwned[t] {
			continue
		}
		advisory = append(advisory, t)
	}

	sort.Strings(caveats)
	return Report{
		Drift:        drift,
		Advisory:     advisory,
		OpaqueWrites: opaque,
		Migrations:   replayed,
		LibraryOwned: sortedKeys(libOwned),
		ParseCaveats: caveats,
	}
}

// codeTables extracts the code's DB write set from the graph edges. It returns the
// resolved tables (each with the ops and owners that reference it) and a count of
// OPAQUE writes — DB boundary edges whose table could not be named (a driver-method
// fallback like ExecContext/call, or a bare-verb fold with no table). The opaque
// count is the lower-bound signal: a table touched only via opaque SQL is invisible
// here, so the resolved set is a lower bound, exactly the db-call frontier.
func codeTables(edges []Edge) (map[string]*tableWrites, int) {
	tables := map[string]*tableWrites{}
	opaque := 0
	for _, e := range edges {
		rest, ok := strings.CutPrefix(e.To, dbBoundaryPrefix)
		if !ok {
			continue // not a DB boundary edge
		}
		f := strings.Fields(rest)
		// A resolved write is "<VERB> <table>": a table-naming verb (upper case)
		// followed by a table token. A method-name fallback (ExecContext/call) is
		// mixed-case and not in the set; a bare-verb fold has no table token. Both
		// are opaque — the table is unknown.
		if len(f) < 2 || !tableNamingVerb[f[0]] {
			opaque++
			continue
		}
		op, table := f[0], f[1]
		w := tables[table]
		if w == nil {
			w = &tableWrites{ops: map[string]bool{}, sites: map[string]bool{}}
			tables[table] = w
		}
		w.ops[op] = true
		if e.From != "" {
			w.sites[e.From] = true
		}
	}
	return tables, opaque
}

// normalizeSet cleans each declared name (so "Public.Outbox" matches the code's
// "outbox") and returns a membership set.
func normalizeSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if c := cleanName(n); c != "" {
			m[c] = true
		}
	}
	return m
}

// sortedKeys returns the keys of m in ascending order, so no map-iteration order
// reaches the output. (Local helper, matching the static packages' idiom — sqlfold
// keeps its own rather than importing across layers.)
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
