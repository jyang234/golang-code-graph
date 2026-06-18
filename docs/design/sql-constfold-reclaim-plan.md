# SQL const-accumulation fold — reclaiming the constant-fragment builder frontier

> **`PROPOSAL`** · exploratory, building the case · _drafted 2026-06-18_

**Status:** phase 1 (the fold + trichotomy + provenance) is **shipped, opt-in** as
`internal/static/sqlfold`, wired into the labeler via `graphio.WithSQLFold`
(`flowmap graph --reclaim-sql` / `flowmap frontier --reclaim-sql`); the committed
fixture `testdata/fixtures/sqlbuildersvc` is its motivating measurement (6 B2
markers → 2 with the fold, the two genuine-dynamic sites correctly retained).
Phase 2 (finite-constant table naming) is **also shipped**: a write's dynamic table
resolves to its complete constant set when provable — the fleet's `s.table ∈
{publishers, subscribers}` fans out into `DELETE publishers` + `DELETE
subscribers` — abstaining on every completeness gap. Phase 3 (the B2a/B2b
disclosure split) is **shipped**: the frontier classifier is fold-aware, so a
folded graph's residual `opaque-db` markers are disclosed as the genuine B2b
consumer-ask while an unfolded graph points the reader at `--reclaim-sql` (which
folds the B2a builder sub-class). **All three phases are now built.** This plan is
the SQL-label analogue of the strict-server
*edge* reclaimer shipped in `frontier-instrumentation-plan.md` (component 3,
`internal/static/reclaim`). It inherits that plan's doctrine wholesale (R1–R4, the
A/B/B2/C taxonomy, opt-in + provenance discipline) and adds the one new soundness
argument a *label* reclaimer needs that an *edge* reclaimer does not. The
motivating measurement is field report §18 (pin `d237b59`), observed-plus-source
on the `event-bus` + `cgate` fleet.

> **Build note (phase 1).** The decision to fold *reads* (per the answer to the
> slice-1 scope question) made the chain-following exactness load-bearing: an early
> implementation compared `types.Type` with `==`, which silently dropped the rest
> of a fluent chain (distinct `*types.Pointer` instances are not pointer-equal), so
> a `SELECT … + dynamicCol` read its clean constant prefix and declared the
> statement *complete* — a false read, the cardinal sin. Fixed with
> `types.Identical` plus an escape check so `complete` means "provably saw every
> write." `TestFoldAbstainsOnDynamicTextSpliceInSelect` pins it.

The framing question this answers for the owner: **a large, dominant slice of the
B2 "opaque SQL" frontier is not dynamic at all — it is a compile-time-constant
statement laundered through a `strings.Builder`. Can we read the verb back out
without ever manufacturing a false read (a false proof of non-mutation)?**

---

## 0. Thesis

Three findings drive the design:

1. **B2 is mostly not dynamic — it is constant SQL laundered through an
   accumulator.** The fleet builds every query from a tiny fluent builder
   (`sqlWriter.Write("SELECT …").Arg(id).Build()`). The verb-bearing fragment is a
   **Go string constant**; it becomes a non-constant `string` only because `Build`
   returns `strings.Builder.String()`, a runtime value. The information needed to
   classify read/write is statically present, one accumulator-hop from the call
   site — the same situation `--reclaim` faced at the oapi `$1` dispatch seam.

2. **The safe direction is asymmetric, and the builder lands on the safe side by
   construction.** Values never enter the statement *text* — they are appended as
   `$N` placeholders by `.Arg`/`.Param`. So a successful fold reconstructs the
   *entire* statement (modulo placeholders the canonical normalizer already maps to
   `?`), not just a prefix. There is no unconstrained text splice to smuggle a
   second statement through, which is exactly what makes the read direction sound
   here where it is *not* sound for `"SELECT 1; " + var`.

3. **This is write-recovery, not just read-shrinking.** The fleet executes
   `INSERT … RETURNING` / `UPDATE … RETURNING` through `QueryRowContext`.
   Classifying by `database/sql` method (`Query*`→read) would mislabel the entire
   `cgate` write surface as reads — a false proof of non-mutation, the cardinal
   sin. Reading the verb off the reconstructed constant keeps those writes on the
   write surface where they belong.

The lever is therefore a sound, opt-in, provenance-tagged **label reclaimer**: it
relabels an opaque `db ExecContext` / `db QueryRowContext` effect with the verb
(and, when also constant, the table) it recovers from the constant accumulation —
or abstains and leaves the effect exactly as opaque as today.

---

## 1. What the frontier actually looks like (the measurement)

The verbatim builder (byte-identical across both services' `storage` packages; a
richer dialect-aware `SQLWriter` in the shared `messaging-relational` package has a
different API — `Literal`/`Table`/`Column`/`Param` — but the **same
`strings.Builder`-laundering property**):

```go
type sqlWriter struct {
  sb   strings.Builder
  args []any
}
func (w *sqlWriter) Write(s string) *sqlWriter { w.sb.WriteString(s); return w }  // s is a CONSTANT at every call site
func (w *sqlWriter) Arg(v any) *sqlWriter {
  w.args = append(w.args, v)
  w.sb.WriteByte('$'); w.sb.WriteString(strconv.Itoa(len(w.args)))               // values → $1,$2,… never inlined
  return w
}
func (w *sqlWriter) Build() (string, []any) { return w.sb.String(), w.args }      // String() is a RUNTIME value
```

A typical call site (`cgate` `GetMessage`):

```go
w := newSQLWriter()
w.Write("SELECT " + messageColumns + " FROM messages WHERE id = ").Arg(id)
query, args := w.Build()                                                     // opaque string var
m, err := scanMessage(s.db.QueryRowContext(ctx, query, args...))             // labeler → db QueryRowContext
```

Two structural facts that matter for the fold:

- **The leading fragment is already a single `*ssa.Const`.** `messageColumns` is a
  `const` column-list, so `"SELECT " + messageColumns + " FROM messages WHERE id =
  "` is folded by Go's own constant evaluator before SSA. The first `Write` arg is
  a constant string the fold reads directly — no need to chase the `+`.
- **The residue is tiny and self-identifying.** `cgate` const-folds end to end. The
  fleet's only genuinely runtime-dynamic statement is `event-bus`'s per-table
  participant store: `"DELETE FROM " + s.table + " WHERE id = "`, where `s.table`
  is a struct field holding one of two string literals (`"publishers"` /
  `"subscribers"`). Its **verb is still constant** (`DELETE`) — see §4.

This is the bulk of the `fitness` disclosure measured in §16 at `4acee55`:
*"unenforceable on 33 route(s) — db ExecContext, db QueryContext, db call …"* —
overwhelmingly constant-fragment-builder sites, not dynamic SQL.

---

## 2. Where it sits in the taxonomy — refining B2

The frontier taxonomy (`internal/static/frontier`, plan §2) bins this as **B2 —
consumer-reclaimable: opaque because the *source* is non-constant**, with decision
D3 = *disclose the ask ("hoist the SQL to a `const`"), no codemod*. §18 shows D3 is
too pessimistic for the dominant sub-shape: the source **is** constant; it is the
accumulator boundary, not the programmer, that breaks the chain. So we split B2:

- **B2a — accumulator-reclaimable (NEW).** Opaque only because a compile-time
  constant statement flows through a string accumulator. **Maintainer-reclaimable**
  by the fold in this plan — the SQL analogue of B (reclaimable structure), not a
  consumer ask.
- **B2b — genuinely consumer-reclaimable.** Opaque because a non-constant *text*
  fragment (a runtime table/identifier in the read direction) enters the statement.
  Stays B2, stays D3's "disclose the ask." This is the honest residue.

`readableDBVerb` (`internal/static/frontier/frontier.go:347`) is the current
discriminator: an upper-case leading token is a verb the labeler read; a mixed-case
method name (`ExecContext`) is the fallback. After the fold, a reclaimed verb reads
as upper-case through the *same* discriminator, so the classifier reclassifies B2a
sites with no change to its rules — exactly how `--reclaim` makes severed B routes
reach their effects.

---

## 3. The doctrine check — a label reclaimer needs a new soundness argument

The edge reclaimers inherit a clean monotonicity proof (plan §1): adding a true
edge can turn `provenAbsent`→`reachable` but never the reverse, so it cannot
manufacture a false proof of absence. **A label reclaimer does not get this proof
for free** — it changes how an effect is *classified*, and the dangerous direction
is concrete:

> The forbidden move is relabeling an effect that **might mutate** as a **read**
> (non-write). That is a false proof of non-mutation — it removes a real write from
> the budget's write surface and can produce a false SATISFIED, the exact
> silent-green the framework exists to prevent.

So the fold inherits R1–R4 from the frontier plan and adds:

- **L1 (asymmetric classification).** The fold may relabel an effect as a **write**
  on weaker evidence than it may relabel one as a **read**. Promoting to write needs
  only a provably-mutating *leading verb*; demoting to read needs the *whole*
  statement to be free of any unconstrained text splice (§4). A write
  classification can only *add* to the write surface (safe direction); a read
  classification *removes* from it (dangerous direction) and carries the heavier
  proof burden.
- **L2 (abstain is always available and always safe).** When the fold cannot meet
  L1's burden for either pole, it emits nothing and the effect stays the opaque
  `db <method>` label it is today — a B2 caution, never a guess. Abstaining is
  strictly the current behavior; the fold can only ever *improve* on it or match it.
- **L3 (descriptive provenance, R3-compatible).** A reclaimed label carries its
  origin (`via: "sql-constfold"`) and a substrate caveat (*"verb reclaimed by
  const-accumulation fold"*), so a verdict that leaned on a reclaimed verb is
  auditable. Unlike the classifier's A/B labels (pure instrumentation, R3), a
  reclaimed verb *does* feed the verdict (it is the write-surface classification),
  so its provenance is mandatory, not optional — the §7 auditability rule (R9)
  applied to labels instead of edges.

---

## 4. The soundness procedure — the verb / placeholder / text-hole trichotomy

The fold reconstructs the statement as a sequence of fragments, each classified by
how it entered the accumulator:

| Fragment kind | Source | Can it carry a `;` / a keyword? |
|---|---|---|
| **constant text** | `Write(<const>)`, `Literal(<const>)` | known — inspect it |
| **placeholder** | `Arg(v)`/`Param(v)` → `$N` (digits only) | **no** — provably separator-free |
| **non-constant text/identifier hole** | `Write(var)`, `Table(var)`, `+ var` | **yes** — unbounded |

The decision procedure, applied after assembly:

1. **Leading verb is in a constant region and is mutating** (`sqlverb.Mutating`) →
   **classify write**, regardless of any later hole. No splice can turn a `DELETE …`
   into a non-write; appending cannot un-write a write. The *table* is named only if
   it too is in a constant region; otherwise the write target is `<dynamic>`.
2. **Leading verb is a read AND every hole is a placeholder** → **classify read**.
   There is no unconstrained text anywhere, so multi-statement smuggling is
   impossible. This is every `cgate` `SELECT … WHERE id = $1`.
3. **Leading verb is a read AND any non-constant text/identifier hole exists** →
   **abstain** (B2b). A dynamic identifier (`Table(s.table)`,
   `"SELECT … " + col`) carries the same smuggling risk as a `+ var` text splice
   (`s.table = "x; DELETE …"`), so the read direction stays closed.
4. **The verb itself is in a hole** (`Write(verbVar)`) → **abstain**. Nothing is
   recoverable.

**Worked example — the residue is still write-classifiable.** `"DELETE FROM " +
s.table + " WHERE id = "` hits rule 1: the leading verb `DELETE` is constant, so the
site is a sound **write with a `<dynamic>` table** — it leaves the
unclassified-might-mutate channel and is charged to the budget, even though
`s.table` stays unnamed. So write-surface *enforceability* (what
`UnclassifiedDBLabel`, `internal/groundwork/fitness/budget.go:158`, measures) reaches
effectively 100% on the fleet; only one site's table name stays dynamic. (If naming
that table is wanted, `s.table ∈ {"publishers","subscribers"}` is a 2-element
constant set the finite-enumeration follow-on of §6 can resolve — both are writes,
so it changes nothing about the verdict.)

This trichotomy is the whole soundness story: **write needs only the leading verb;
read needs the absence of any unconstrained splice.** The builder idiom makes the
read sites all-placeholder and the residue write-verbed, so the fleet is fully
classifiable.

---

## 5. Implementation — a structural `strings.Builder`-laundering fold

**Convention-free over per-builder config.** §18 already shows two builder APIs
(`sqlWriter.Write`, `SQLWriter.Literal/Table/Column/Param`) in one fleet — proof
that a recognizer keyed on public method *names* will keep growing. Both builders
share the structural property that matters: they append to an embedded
`strings.Builder` and return the receiver. So the fold keys on that structure, not
the names.

**Per-method accumulator summary.** For each method invoked in a fluent chain on a
builder value, summarize its effect on the receiver's `strings.Builder` field:

- **text-append**: body does `recv.<sb>.WriteString(param)` / `WriteByte(param)`
  where `param` is the method's string argument, and returns the receiver →
  contributes that argument's value (constant text, or a text-hole if non-constant).
- **placeholder-emit**: body appends a `$`/digit token derived from a counter, not
  from a value (`Arg`, `Param`) → contributes the normalizer's placeholder; the
  value is irrelevant to text.
- **identifier-append** (`Table`, `Column`): a text-append whose argument sits in an
  identifier position — same rules, but a non-constant argument is a text-hole under
  rule 3 (it can still smuggle), *unless* the verb is already pinned to a mutating
  value under rule 1.
- **build/terminal**: returns `<sb>.String()` (`Build`, `String`) — the fold target.
- **anything else** → unknown → **abstain** (L2).

`Build`/`String`'s result, traced to a `db` boundary call's query argument (the
existing `dbLabel` path, `internal/static/graphio/labels.go:45`), triggers the fold;
the reconstructed constant skeleton runs through the **one** canonical normalizer
(`internal/canon/sql`, shared with the behavioral op key) so a reclaimed label can
never disagree with the op key on verb or table.

**Visibility / escape gate (the L1/L2 admission ticket).** The fold emits a label
only when **every** write to the builder instance is statically visible and
classified. If the builder value escapes to an opaque callee that could
`w.Write(someVar)`, or any fragment is unknown, abstain. This is the label analogue
of the strict-server reclaimer's flow check (`reclaim.flowsTo`): soundness rests on
seeing the *whole* accumulation, not on the builder's name.

**Assembly order is deterministic.** Fragments are concatenated in the fluent-call
order — the def-use chain on the builder receiver — with CFG order and a canonical
tie-break for any multi-statement method body. Pure function of the SSA; no clock,
no corpus.

**Placement.** A new `internal/static/sqlfold` package (mirroring
`internal/static/reclaim`), consumed by `dbLabel`. It does **not** go through
`graphio.ApplyReclaimers` (`internal/static/graphio/graphio.go:427`) — that folds
*edges*; this fold produces a *label*. The wiring is opt-in (a flag, e.g.
`--reclaim` extended to cover label reclaimers, or a sibling `--reclaim-sql`), the
default `Build` and every committed golden unchanged (D2 analogue).

**Provenance plumbing (L3).** The boundary edge carries a `via: "sql-constfold"`
tag (the groundwork decoder already round-trips a per-edge `via`,
`internal/groundwork/graph`), and `provenanceCaveats`
(`internal/groundwork/review/provenance.go`) gains a const-fold caveat alongside
`ReclaimCaveat()`, so the substrate line discloses a fold-informed verdict.

---

## 6. Phasing

1. **Structural fold + trichotomy (this plan's core). ✅ DONE (opt-in).** The
   `strings.Builder` summary, the verb/placeholder/text-hole decision, wired into
   `dbLabel` with the `via` tag (`sqlfold.Via`) and the substrate caveat
   (`graph.SQLFoldCaveat`). Convention-free, catches both fleet builders and inline
   `strings.Builder`; gets `cgate` end-to-end and the `event-bus` write residue (as
   write/`<dynamic>`-table) in one slice. Opt-in; default `Build` and every golden
   unchanged. Shipped as `internal/static/sqlfold` with the determinism test and the
   `sqlbuildersvc` fixture spanning all five outcomes (read / write-via-QueryRow /
   dynamic-table write / branched write / abstain) plus the dynamic-splice read
   guard.
2. **Finite-constant table naming. ✅ DONE (opt-in).** A write whose table is a
   hole resolves to the table's complete constant set via `resolveConstSet` /
   `resolveField` (`internal/static/sqlfold/resolve.go`): a constant, a Phi union, or
   a struct-field load proven all-constant by four completeness gates — (a) every
   `FieldAddr` of the field used only as a constant store or a load, (b) no
   whole-struct overwrite, (c) init-before-escape dominance (no zero-value leak),
   and a set-size bound. A resolved set fans out into one write edge per table
   (`DELETE publishers` + `DELETE subscribers`); any gate failure leaves the target
   dynamic. Runs ONLY in the write branch, so it is verdict-neutral and
   safe-direction — a naming miss can over-list or under-name a target but never
   hide a write. `TestFoldResolvesFiniteConstantTableSet` and
   `TestFoldAbstainsOnNonConstantTableField` pin both directions.
3. **Disclosure reconciliation. ✅ DONE.** The frontier classifier
   (`internal/static/frontier`) carries a fold-aware `Folded` signal (set by graphio
   from the `sqlfold.Via` provenance on boundary edges). Unfolded, an `opaque-db`
   marker's hint points the reader at `--reclaim-sql` (which folds the B2a
   constant-fragment-builder sub-class for free); folded, the remaining `opaque-db`
   markers ARE the genuine B2b residue and the hint becomes the consumer ask ("make
   the statement a `const`"). The `Report`/`Render` carry `SQLFolded` so the B2 count
   reads as the residue, not the union — an agent reading the data (not just the
   prose) sees the regime. No committed golden churns: no default-build graph carries
   an `opaque-db` marker, and the embedded section is always the unfolded build.
   `TestOpaqueDBDisclosureIsFoldAware` pins both regimes.

Each phase ships with a determinism test and a `canon/sql` fuzz-corpus extension
(per the repo's determinism-test rule), and a fixture mirroring the two builder
shapes so the fold's prevalence gate (D4, breadth) is met by a *class* of services,
not one bespoke builder.

---

## 7. Determinism risk register

| Risk | Guardrail |
|---|---|
| Fold relabels a might-mutate effect as a **read**, hiding a write (false SATISFIED) | L1: read classification requires *no* unconstrained text/identifier hole (rule 3); placeholders only. Write needs only the leading verb. Asymmetric burden, reviewed per-PR. |
| Builder escapes to an opaque writer that appends non-constant text | Visibility gate: emit only when every write to the instance is visible and classified; else abstain (L2). |
| Reclaimed verb disagrees with the behavioral op key | Both derive verb/table from the **one** `canon/sql` normalizer; parity is structural, not copied. |
| Per-method summary mis-models an append, fabricating a fragment | Summary is a pure SSA read; unknown method shape → abstain, never guess. Test-backed against both fleet builder shapes. |
| Assembly order varies run-to-run | Fluent-call/def-use order with a canonical CFG tie-break; determinism test + fuzz corpus. |
| Fold output churns committed goldens | Opt-in behind a flag (D2 analogue); default Build untouched; reclaimed graph is an explicit diffable superset. |
| Verb reclaimed but verdict can't disclose it | L3: `via: "sql-constfold"` on the edge + a substrate caveat; mandatory because the verb feeds the verdict. |

---

## 8. Decisions

- **D1 — Split B2 into B2a (accumulator-reclaimable, maintainer-side) and B2b
  (text-hole, consumer-side).** The dominant sub-shape is reclaimable by us; only
  the genuine text-hole residue stays the D3 "hoist to const" ask. *Fork: leave B2
  monolithic and treat the fold purely as a labeler improvement with no taxonomy
  change — simpler, but loses the honest "what's left for the consumer" signal.*
- **D2 — Convention-free structural fold first, not a name-keyed recognizer.** Two
  builder APIs in one fleet make a name list a treadmill; both share the
  `strings.Builder`-laundering property, so the structural summary catches both. *A
  configured per-builder descriptor stays available as a fallback for a builder that
  launders through something other than `strings.Builder` (a `bytes.Buffer`, a raw
  `[]byte`).*
- **D3 — Label reclaimer, opt-in + provenance-tagged, NOT folded through
  `ApplyReclaimers`.** It produces a label, not an edge, so it needs its own
  soundness argument (§3, L1–L3) and its own `via`; reusing the edge-fold machinery
  would conflate two different operations. Opt-in mirrors the `--reclaim` precedent.
- **D4 — Prevalence gate, same two qualitative bars as the frontier plan.** Gate 1
  (breadth): the constant-fragment builder is a recurring shape across a class of
  `lib/pq`/`database/sql` services, not one codebase — met by the two distinct
  fleet builders. Gate 2 (soundness): the fold is a *local, statically-provable*
  reconstruction (all writes visible, verb in a constant region), reviewable in
  isolation. Both met; promotion to default-on stays gated on a real-corpus
  soundness bake, like every reclaimer.

---

*Companion artifacts (to build): a `testdata/fixtures` pair mirroring the
`sqlWriter` and `SQLWriter` builder shapes; `internal/static/sqlfold` + its
determinism test; a `canon/sql` fuzz-corpus extension for the reconstructed
skeletons. Prior art: `frontier-instrumentation-plan.md` (the edge-reclaimer
doctrine this inherits, R1–R4 / D1–D4), field report §14/F-B (the method-name
classification rejection this plan vindicates), §16 (the 33/10 measurement), §18
(the constant-fragment-builder characterization). Pin: `d237b59`.*
