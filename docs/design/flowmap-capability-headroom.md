# flowmap capability headroom — sound, deterministic extensions (with prototype evidence)

> **`PROPOSAL`** · exploratory, building the case · _drafted 2026-06-21_

**Status:** a headroom pass that pressure-tested flowmap's *scope* boundaries — explicitly **not**
its soundness or determinism, which are untouchable. The finding: most of what currently reads as
"out of scope" is roadmap, not a wall — pushable while *fully* respecting determinism, soundness,
and fail-closed. Two items carry prototype evidence: the schema-drift cross-check is **fully
prototyped on two real services** (event-bus + cgate), and value-flow/taint is **spiked with a
runnable PoC** (§3). In both, building it first corrected a pre-build assumption — the schema check's
false-positive class, the taint spike's pessimism about the field-flow wall — *before* it became
feedback. That is the whole reason to prototype on our side first. Ranked by soundness × utility
below. Substrate claims verified against pin `85ca0a9`.

## The frame

The prime directive constrains **how** — sound, deterministic, fail-closed, disclose-don't-guess —
not the current feature set. The tool's own over-approximate reachability is the existence proof:
*"this route cannot reach a write"* is a sound, deterministic, imprecise-but-honest claim. Anything
in that same shape — a sound absence-proof + an over-approximate candidate, or a deterministic
set-comparison of two declared surfaces — is admissible. The real boundary is *what's undecidable
even over-approximately* (genuine runtime values), which is **narrower** than today's scope. Every
item below stays inside the directive; none asks the tool to guess.

---

## 1 · Schema-drift cross-check — SHIPPED

> **Shipped** as `internal/static/schemadrift` + `flowmap schema-drift` (with `--gate`
> and build-fresh). See [`schema-drift-check-plan.md`](schema-drift-check-plan.md) for
> the build record; the `testdata/fixtures/schemadriftsvc` fixture reproduces the
> `provisioning_outbox` completeness finding below on real graphio-emitted labels.

**Capability.** Cross-check the code's DB targets — *already* in the emitted IR as boundary labels
(`boundary:db INSERT event_type_subscriptions`) — against the tables the migrations define. A
deterministic post-process on the existing graph + a parsed schema; it never touches the graph
build, so it carries **zero risk** to the analyzer's determinism or soundness. It catches the
`relation "X" does not exist` class — a documented Koalafi deploy hazard (a library/version bump
that outruns the migrate step leaves pods writing a table the schema lacks).

**We built it and ran it on event-bus + cgate.** Results:

- **Sequence replay is load-bearing.** event-bus creates `queue_messages`/`dead_letter_messages`
  (V3) then drops them (V8); the check replays forward migrations in version order, applies
  CREATE/DROP, and excludes `*_rollback.sql`. The dropped tables correctly drop out of the schema
  set — a naive `CREATE TABLE` grep would have kept them.
- **Sound drift direction works.** A resolved/folded `db <OP> <table>` label ∉ the defined schema ⇒
  flagged. cgate: **clean** (4 code tables = 4 Flyway tables) — a true negative, no false fire on a
  well-formed service.
- **The false-positive class the prototype caught (verified).** event-bus's first run flagged
  `provisioning_outbox` as drift. It is **not** drift — it is **library-owned**: the `outbox`
  library auto-migrates it via its `Migrator`; *no* Flyway script creates or even mentions it
  (verified). The naive "∉ Flyway" check therefore false-fires on the **standard outbox/inbox
  pattern** — which this codebase uses everywhere. **The defined-schema set must be Flyway ∪
  library-owned tables** (declared like a classify hint, or discovered from the library's `Migrator`
  DDL). With `provisioning_outbox` folded in, event-bus is **clean**.
- **Noise boundary — the reverse direction is advisory, not sound.** "Schema defines table Y, code
  never touches Y" false-fires (`publishers`, `subscribers`) because those writes are non-constant
  SQL (`db call`), so the code-side set is a **lower bound**. Report it advisory-only.

**Soundness profile (earned by building):**

- **Drift is sound iff the schema set is complete** (Flyway ∪ library-owned). The prototype proved
  completeness is the load-bearing condition, not an afterthought.
- The check **inherits the db-call frontier**: a table touched only via non-constant SQL is unlabeled,
  so a drift on *that* table is missed. "No drift" means "no drift among resolved writes" — pair it
  with the existing db-call disclosure, exactly as the budget/ratchet already do.
- Deterministic throughout: migration replay (version-ordered), graph parse, set-diff. Pure
  post-process — the graph build is untouched.

**Integration sketch.** A `flowmap`/groundwork post-process reading the emitted graph + the migration
dir + a declared/discovered library-owned set. Prototype caveat: it uses a regex DDL scan; production
swaps in a real DDL parser — but that changes neither the algorithm nor the findings. Code in
Appendix A.

## 2 · Effect-kind recognizers — trivial, additive

`db`/`bus`/`http` is what's *built*, not the limit. Other external write surfaces — object storage,
a cache, a non-HTTP RPC peer — render today as disclosed boundaries (named, not budget-counted).
Adding recognizers (or `classify` categories) for them is additive, deterministic, and sound — a
declared boundary in the same shape as the existing five hints, with zero soundness cost.

> **Shipped (the `reclaim-topic` sibling).** The `reclaim-sql` analog for bus targets is built:
> `graphio.WithTopicFold()` / `flowmap graph|frontier --reclaim-topic`. A PUBLISH/CONSUME whose topic
> resolves to a finite, provably-complete constant set is named (one edge per topic, tagged
> `via=topic-constfold`) instead of `<dynamic>`, shrinking the dynamic-bus residual. It reuses the
> SQL fold's const-set resolver (`sqlfold.ConstStringSet`) — one source of truth for the completeness
> discipline — and is *safer* than the SQL fold: the topic is a verdict-neutral target name, so it
> can never move a pole. Sound-or-abstain; off by default. Fixture: `testdata/fixtures/topicfoldsvc`.

The remaining recognizers (object storage, cache, non-HTTP RPC) stay additive pattern work — low
effort, no risk, not yet built.

## 3 · Value-flow / taint — spiked (PoC built); feasible medium build, precision-gated

**We built it.** A ~150-line forward value-flow PoC on the `go/ssa` substrate (no `go/pointer`),
run on a fixture mirroring the real PII shape (`Event{Recipient{Email,Phone,CustomerID}}`,
`MergeData map[string]string`). Every result is sound:

| Case | Result |
|---|---|
| direct `sink(source())` | **FLOW** (could-flow candidate) |
| interproc `source() → relay(param) → sink` | **FLOW** (arg → param) |
| struct-field round-trip `b.v = source(); sink(b.v)` | **FLOW** |
| value into a map `m[k] = source(); sink(m[k])` | **ABSTAIN** (frontier — refuses to claim no-flow) |
| `source()` never reaches the sink | **NO-FLOW (sound proof)** |

The model is reachability's asymmetry: *cannot-flow* = proof (the valuable gate — "this PII
cannot reach this log/boundary"), *could-flow* = candidate to verify.

**The spike corrected our pre-build pessimism.** The struct-field round-trip is **covered** — via a
global (struct-type, field-index) field-set match, the same `computeFieldSet` trick the tool already
uses for SQL, *without* `go/pointer`. Since the real PII path is struct-field-threaded
(`event.Recipient.Email → SendEmail`, `event.Recipient.CustomerID → logger.Info`), the motivating use
case sits **inside** the coverable region. The wall is narrower than feared: **maps / interfaces /
channels / reflection / dynamic dispatch**, where the PoC abstains soundly (never a false no-flow). On
real cgate: `Recipient.*` (struct fields) coverable; `MergeData` (map) abstains.

**Founded on existing passes** — forward `ssa` def-use, the VTA call graph (interproc), and the
field-set. So it is a **medium build, not a research lift**: a build-time SSA pass where
`resolveConstSet`/`computeFieldSet` already live, emitting a new annotation (the `Via`/`Boundary` field
is the template) — not a graph post-process (the emitted `Node`/`Edge` carry no value info).

**What the PoC did NOT settle** (honest gaps, priority order):

1. **Precision on real multi-instance code — the gate before production.** The field-set is
   type+index-*global* (field-insensitive to instance), so on a service with many struct instances
   *could-flow* will over-fire. The PoC had one instance; the noise on real cgate/event-bus is
   unmeasured. **Measure it before shipping** — if it over-fires, scope down (flow-sensitive field
   handling, or a narrower declared source set).
2. **Interprocedural *return*-flow is unimplemented.** The PoC does arg → param only; `x := getEmail(e);
   log(x)` needs return-tainting. Standard, more code.
3. **Sources/sinks were functions, not declared field-reads.** Production marks sensitive *fields* as
   sources (a declared list, like classify hints); the PoC proved the field *mechanism*, not the marking.

**Verdict: GO for a fuller PoC on real code, measuring precision** — not a blind production build.
Scope: declared sensitive sources + sink set; forward def-use + interproc (arg → param **and** return);
the field-set for struct fields; abstain at maps/interfaces; a build-time SSA pass emitting a new
annotation + a `must-not-flow` gate. `go/pointer` is an **optional later tier** for the map/interface
frontier — explicitly not needed for v1. The use case that justifies it: a sound *"this PII field
cannot reach this log/boundary sink"* gate, enforcing the no-log-PII rule.

## The one true boundary (no sound headroom)

Resolving genuine **runtime values** — which LaunchDarkly flag or env-driven branch actually executes —
is undecidable statically; over-approximation (both branches live) *is* the sound answer. The only
sliver is const-flag pruning (sound const-propagation). This one stays a wall, correctly.

## Register

Every item is sound by construction — a deterministic set-diff (1), a declared recognizer (2), or a
fail-closed value pass (3) — and none asks the tool to guess past a blind spot. The render-correctness
fix and disclosure refinements from the same pass live in the addendum to the C3-bands FR
(`upstream-flowmap-c3-bands.md`). Same register throughout: extend *sound* resolution, improve
*disclosure*, never lower the soundness bar.

## Appendix A — the schema-check prototype (stdlib-only, runs on real services)

`go run schema_check.go <graph.json> <migrations-dir> [library-owned,tables]`. Core logic:

```go
// code DB targets: from resolved/folded boundary labels. "db call" (non-constant SQL)
// carries no table -> opaque, the lower-bound signal.
func codeTables(g graph) (map[string]ops, int) {
    for _, e := range g.Edges {
        rest, ok := strings.CutPrefix(e.To, "boundary:db "); if !ok { continue }
        f := strings.Fields(rest); op := strings.ToUpper(f[0])
        if op == "CALL" { opaque++; continue }
        if !writeOrRead[op] || len(f) < 2 { continue } // skip tx control / table-less
        add(tables, cleanName(f[1]), op)
    }
}

// defined schema: replay forward migrations in version order, apply CREATE/DROP,
// EXCLUDE *_rollback.sql. Final set = creates − drops.
func schemaTables(dir string) map[string]bool {
    for _, f := range sortedByVersion(forwardOnly(dir)) {     // V<n>__*, then R__*
        sql := read(f)
        for _, m := range reCreate.FindAll(sql) { schema[cleanName(m)] = true }
        for _, m := range reDrop.FindAll(sql)   { for _, t := range split(m, ",") { delete(schema, cleanName(t)) } }
    }
}

// defined := Flyway ∪ library-owned (outbox/inbox auto-migrated — declared or Migrator-discovered)
for _, t := range libraryOwned { schema[t] = true }

// SOUND: code label ∉ defined schema  ->  `relation does not exist` risk
// ADVISORY (noisy, lower-bound): defined ∉ code labels  ->  unused / opaque-SQL-touched
```

Measured: **event-bus** — 11 forward migrations replayed; schema = {event_type_subscriptions,
event_type_template_subscriptions, event_type_templates, event_type_versions, event_types, publishers,
subscribers} (queue tables correctly dropped); code = those + `provisioning_outbox`; drift after
folding the library-owned `provisioning_outbox` = **none**; advisory = publishers, subscribers (opaque
SQL). **cgate** — 3 migrations; code tables = schema tables; **clean** both directions.
