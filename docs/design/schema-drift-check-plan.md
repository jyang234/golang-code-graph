# Schema-drift cross-check — implementation plan (§1 of the headroom analysis)

> **`DESIGN RECORD`** · phases 1–4 shipped; kept as the rationale · _reviewed 2026-06-21_

**Status:** **SHIPPED** as `internal/static/schemadrift` + the `flowmap schema-drift`
subcommand. The core (Phase 1), CLI (Phase 2), config + a real-service fixture
(Phase 3), and the `--gate` / build-fresh CI ergonomics (Phase 4) all landed; the
motivating fixture `testdata/fixtures/schemadriftsvc` reproduces the prototype's
`provisioning_outbox` completeness finding on real graphio-emitted labels. Two items
from the original Phase 4 stay deferred-by-design: a real DDL parser (the regex scan
held; a parser is a v2 swap behind the same interface, not worth a new dependency in
a determinism-critical tree until the corpus demands it) and Migrator-discovery of
the library-owned set (needs a concrete library's DDL; declared lists suffice today).
Kept as the design record: it settles where the check lives, the package shape, and —
the load-bearing part — the soundness/completeness contract. Substrate facts cited
against pin `85ca0a9`.

## What it is (recap)

A deterministic post-process: diff the DB tables the code *writes* — already in
the emitted graph as `boundary:db <OP> <table>` labels — against the tables the
migrations *define*. A code label ∉ the defined schema is the
`relation "X" does not exist` deploy-hazard class. It **never touches the graph
build**, so it carries zero risk to the analyzer's determinism or soundness — the
whole appeal of starting here.

## Where it lives

**A `flowmap schema-drift` subcommand, core logic in a new
`internal/static/schemadrift` package** — modeled cell-for-cell on `frontier`:

| Concern | `frontier` (the template) | `schema-drift` (new) |
|---|---|---|
| CLI dispatch | `cmd/flowmap/main.go:51` switch → `cmdFrontier` (`:303`) | add `case "schema-drift":` → `cmdSchemaDrift` |
| Core package | `internal/static/frontier` (`Classify`/`Summarize`/`Render`) | `internal/static/schemadrift` (`Check`/`Render`) |
| Input decoupling | `frontier.Input` built from the graph (no `analyze` dep) | `schemadrift.Input` built from the graph + parsed migrations |
| JSON output | `canonjson.Marshal(rep)` | same — deterministic canonical JSON |
| Human output | `frontier.Render(name, algo, rep)` | `schemadrift.Render(...)` |
| Tests | `frontier_test.go` over `testdata/fixtures/strictsvc` | `schemadrift_test.go` over a new fixture |

**Why flowmap, not groundwork.** It is a *measurement/disclosure* like `frontier`,
not a verdict gate like `groundwork verify`/`fitness`; and flowmap already owns
graph-consuming post-processes (`frontier`, `diff`). groundwork only reads
committed graphs for the obligation/ratchet world. (Per the headroom doc this can
graduate into a groundwork *gate* later — §"Gate vs. disclosure" below.)

**Input mode — read the emitted graph, don't rebuild.** Primary form:

```
flowmap schema-drift --graph <graph.json> --migrations <dir> [--reclaim-note]
```

Reading the *already-emitted* `graph.json` (via the existing `loadGraphJSON`
pattern, `cmd/flowmap/main.go:282`, decoding into `graphio.Graph` with
`DisallowUnknownFields`) makes "the graph build is untouched" literally true and
testable. Recommend the input be the **`--reclaim-sql` graph** so the resolved
table set is maximal and the opaque residual minimal (the soundness lever, below).
A build-fresh convenience (`schema-drift <dir>`, like `frontier`) is an optional
add, not v1.

## Algorithm (three deterministic stages)

Lifted from Appendix A of the headroom doc, hardened:

**1 · `codeTables(g graphio.Graph)` — the code's write set (a lower bound).**
Walk `g.Edges`; for each `To` with prefix `boundary:db `, split off `<OP> <table>`.
- `op == "CALL"` or a driver-method label (`ExecContext`, …) or `<dynamic>` ⇒ **opaque**:
  no table, increment an `OpaqueWrites` counter, do **not** add to the set. This is
  the db-call frontier — the code set is honestly a lower bound.
- a real verb + table ⇒ add `{table → ops}`.

Reuse the op/table source of truth: the label is built by `sqlOpTable` →
`canon/sql.Normalize` (`internal/static/graphio/labels.go:129`,
`internal/canon/sql/sql.go`). The check must parse the table the **same** way it
was emitted — cite that parity in a comment and pin it with a test, per the
one-source-of-truth tenet (CLAUDE.md). Cleaning/lower-casing goes through
`sql.Normalize`, not a private copy.

**2 · `schemaTables(dir, libraryOwned)` — the defined schema (must be complete).**
Replay forward migrations in version order, apply CREATE/DROP, exclude
`*_rollback.sql`:
- order files by version (`V<n>__*`, then `R__*`); **exclude** `*_rollback.sql`;
- apply `CREATE TABLE [IF NOT EXISTS] <name>` ⇒ add; `DROP TABLE [IF EXISTS] <a>, <b>` ⇒ delete;
- final set = creates − drops, **∪ libraryOwned** (the outbox/inbox tables that no
  Flyway script creates — the false-positive class the prototype caught).

**3 · set-diff (two directions, asymmetric trust).**
- **Drift (sound assertion):** `t ∈ codeTables ∧ t ∉ schemaTables` ⇒ flag. This is
  the deploy hazard.
- **Advisory (lower-bound, noisy):** `t ∈ schemaTables ∧ t ∉ codeTables` ⇒ report
  advisory-only — it false-fires whenever a table is written only via opaque SQL
  (`publishers`, `subscribers` in event-bus). Never a gate signal.

## Soundness contract — the completeness conditions are load-bearing

The prototype's lesson: *completeness of the defined-schema set is the soundness
condition, not an afterthought.* State it precisely, because the two failure
directions are not symmetric:

- **A drift flag is sound (no false positives) iff `schemaTables ⊇` every real
  table.** A *missed* `CREATE`, or an *incomplete* `libraryOwned` set, drops a real
  table from `schemaTables` and produces a **false drift** — exactly the
  `provisioning_outbox` false-fire. So the defined-schema set must be declared
  complete, and when the DDL scan cannot confidently parse a migration it must
  **fail closed** (error/abstain on that file) rather than emit a partial set that
  silently fabricates drift.
- **A *missed* `DROP` is the subtler hazard.** It leaves a phantom (already-dropped)
  table in `schemaTables`, which can *mask* a real drift — code still writing a
  dropped table is precisely the `relation does not exist` case. DROP-parsing
  completeness therefore guards the *absence* claim ("no drift") and deserves its
  own test (the event-bus `queue_messages` V3-create/V8-drop sequence is the
  fixture for it).
- **The check inherits the db-call frontier.** A table touched only via opaque SQL
  is never labeled, so a drift on *that* table is invisible. "No drift" means **"no
  drift among resolved writes"** — the report must disclose `OpaqueWrites` count
  alongside the verdict, exactly as the budget/ratchet pairs counts with the
  db-call disclosure. Running on the `--reclaim-sql` graph shrinks this residual.
- **Determinism throughout:** version-ordered replay (a total order on filenames),
  graph parse, set-diff. All output collections sorted on intrinsic keys (table
  name) before emit — reuse `setutil.SortedKeys`
  (`internal/groundwork/setutil/setutil.go`); no map-iteration order reaches output.

## Config — declaring the schema source

Extend `StaticConfig` (`internal/config/config.go`, alongside `DeclaredBlindSpots`)
with a small sub-config, following the existing declared-hint idiom:

```go
type StaticConfig struct {
    // …existing…
    SchemaCheck SchemaCheckConfig `yaml:"schemaCheck,omitempty"`
}

type SchemaCheckConfig struct {
    MigrationsDir      string   `yaml:"migrationsDir,omitempty"`      // e.g. "db/migrations"
    LibraryOwnedTables []string `yaml:"libraryOwnedTables,omitempty"` // outbox/inbox auto-migrated
}
```

CLI flags (`--migrations`, `--library-owned a,b`) override config. The
`libraryOwnedTables` list is the v1 mechanism for the Flyway ∪ library-owned union;
Migrator-DDL *discovery* of that set is a later tier (it needs the library's own
DDL, out of scope for v1).

## DDL parsing — regex v1, fail-closed; real parser later

There is **no DDL parser in the tree** — `canon/sql.Normalize` is a DML
tokenizer (SELECT/INSERT/…), not DDL, and `go.mod` carries only stdlib +
otel + `golang.org/x/tools`. v1 uses a small, *conservative* CREATE/DROP scanner
in `schemadrift`:
- recognize the common Flyway/pg forms (`CREATE TABLE [IF NOT EXISTS]`,
  `DROP TABLE [IF EXISTS] a, b`, quoted/schema-qualified names normalized through
  the same name-cleaning as the code side);
- **fail closed:** if a migration file contains DDL the scanner does not
  confidently recognize as table-affecting, surface it (a parse-caveat in the
  report) rather than silently under-counting — an unparsed `DROP` is the unsound
  direction.

Later tier (only if it grows): swap in a real parser (`pg_query_go` or similar).
The headroom doc is explicit that this changes *neither the algorithm nor the
findings* — so it stays a v2 swap behind the same interface, not a v1 blocker.

## Output shape

A `Report` mirroring `frontier.Report`, JSON via `canonjson.Marshal`, human via
`Render`:

```go
type Report struct {
    Drift        []DriftItem `json:"drift"`          // SOUND: code writes, schema lacks
    Advisory     []string    `json:"advisory,omitempty"` // lower-bound: schema defines, code never touches
    OpaqueWrites int         `json:"opaque_writes"`   // db-call frontier residual (disclosure)
    Migrations   int         `json:"migrations"`      // forward files replayed
    LibraryOwned []string    `json:"library_owned,omitempty"`
    ParseCaveats []string    `json:"parse_caveats,omitempty"` // fail-closed DDL notes
}

type DriftItem struct {
    Table string   `json:"table"`
    Ops   []string `json:"ops"`   // sorted: INSERT/UPDATE/…
    Sites []string `json:"sites"` // sorted owners (edge.From)
}
```

Exit code: v1 is a **disclosure** (exit 0, drift in the report). A `--gate` flag
(non-zero exit on non-empty `Drift`) is the bridge to CI — see below.

## Fixtures & tests

**New fixture `testdata/fixtures/schemadriftsvc`** mirroring the event-bus shape
that taught the prototype (`.flowmap.yaml` + Go service + `db/migrations/`):
- writes to `event_types` (defined by a migration) — clean;
- writes to `provisioning_outbox` — **library-owned**, no migration creates it;
  declared via `schemaCheck.libraryOwnedTables`; proves the false-positive is
  suppressed;
- a `queue_messages` create-then-drop pair across two versioned migrations plus a
  `*_rollback.sql` that must be *excluded* — proves replay ordering and DROP;
- one genuinely drifted table (code writes it, no migration, not library-owned) —
  proves the sound flag *fires*;
- an opaque (`db call`) write — proves it lands in `OpaqueWrites`, not `Drift`.

**Tests (`schemadrift_test.go`), each pinning a soundness claim:**
1. golden `Report` over the fixture (clean drift, expected advisory, opaque count);
2. **the outbox false-positive**: with `libraryOwned` empty it flags
   `provisioning_outbox`; with it declared, clean — pins the completeness condition;
3. **DROP/replay**: `queue_messages` correctly absent from the schema set;
   `*_rollback.sql` ignored;
4. **the flag fires**: the genuinely-drifted table appears in `Drift`;
5. **determinism**: shuffle `g.Edges` and migration-file read order → byte-identical
   `canonjson` output (the CLAUDE.md obligation for any new ordering path);
6. **op/table parity**: a property test that `codeTables` parses the table the same
   way `graphio` emitted it (one-source-of-truth guard).

As shipped, these became unit tests in `schemadrift_test.go` (synthetic labels) plus
`fixture_test.go` (the real analyzer + `graphio.Build` over the fixture) — the latter
is the one-source-of-truth/parity guard on a genuinely emitted graph. The fixture is
analyze-only, so it runs under `make test` via the package test (the `strictsvc`
pattern), NOT the `fixture` Make target (which gates the behavioral-flow goldens);
the original plan's "wire into the fixture target" was corrected to match how
analyze-only fixtures already work. `make verify` green at the end of every phase.

## Phasing (all shipped)

1. ✅ **Core package** `internal/static/schemadrift` (`schemadrift.go`,
   `migrations.go`, `render.go`): migration replay+scan, `codeTables`, set-diff,
   `Report`. Unit + determinism tests. No CLI.
2. ✅ **CLI** `flowmap schema-drift --graph --migrations`: `loadGraphJSON` →
   `graphEdges` adapter → `Check`, JSON (`canonjson`) + human `Render`. `usage()` entry.
3. ✅ **Config** `StaticConfig.SchemaCheck` + flag override; the `schemadriftsvc`
   fixture + `fixture_test.go` real-graph proof.
4. ✅ **CI ergonomics** `--gate` (non-zero exit on drift) + build-fresh
   (`schema-drift [dir]`, omitting `--graph`, reusing `--algo`/`--reclaim-sql`).
   ⏸ **Deferred-by-design:** a real DDL parser (regex held; v2 swap behind the same
   interface — no speculative dependency in a determinism-critical tree) and
   Migrator-DDL discovery of the library-owned set (declared lists suffice; discovery
   needs a concrete library target).

## Decisions (resolved as built)

1. **Gate vs. disclosure.** Disclosure is the default (exit 0, like `frontier`);
   `--gate` makes drift a non-zero exit for CI, emitted *after* the report so the
   failure ships its evidence. Both shipped.
2. **DDL parsing depth.** Regex/scan, fail-closed — the prototype's findings held
   under it, and a real parser stays a reversible v2 swap behind the interface.
3. **Library-owned set source.** Declared list (`--library-owned` /
   `static.schemaCheck.libraryOwnedTables`); Migrator-DDL discovery deferred.
4. **Read emitted graph vs. build fresh.** Both: `--graph` reads an emitted graph
   (keeps "build untouched" literally true) and is the default story; omitting it
   builds fresh from `[dir]` for the one-step CI form.
