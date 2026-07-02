# Remediation review — 2026-07 audit resolution

> **`ACTIVE`** · verification of the remediation landed for
> `docs/audit/2026-07-code-quality-audit.md` · reviewed at commit `b1d566b`
> (merge of PR #89), `make verify` **green** at review time.
>
> This document records what an independent adversarial review found when
> checking each audit finding's fix against the audit's own demands (fix +
> regression test + comment honesty). It documents **findings only** — no code
> is changed by this review. Items marked **[reproduced]** were empirically
> confirmed against HEAD during this review (via a scratch checkout or a
> `go test -overlay` probe; the repo itself was never modified); items marked
> **[code-verified]** were confirmed by direct code reading at HEAD.

---

## 1. Headline verdict

The remediation is substantially real. Every one of the audit's reproduced
PoC shapes is closed and regression-locked, all 14 HIGHs landed, the drift
class got genuine one-home-plus-parity-guard treatment, and the two
self-review commits (`578613c`, `6427ed1`) caught real defects in the first
drafts of the fixes. `make verify` is green at `b1d566b`, and the review
found **no forged-artifact path and no new nondeterminism reaching a golden**.

That said, the work is not as complete as the tracking plan claims:

1. **Three findings retain live residual defects in their own failure class**
   — C-2 (false SATISFIED still reachable via a closure/pointer-mediated
   `err` reassignment), C-7/M-26 (the `1..*` loop marker is still discarded
   on the Concurrent/Unordered promotion path), and M-24 (the
   quoted-identifier fix breaks the SQL normalizer's own idempotence
   invariant — the nightly fuzz gate fails in under a second).
2. **The LOW list (§5) was silently under-delivered.** The plan checks all
   three LOW groups `[x]` and records exactly one deferral; in reality
   **~17 of the ~40 line items were skipped with no deferral note**.
3. **Several demanded regression tests were not shipped** even where the fix
   itself is sound (C-1's `must_not_reach`-over-the-seam assertion, H-4's
   Build-level fixture, C-2's CANT-PROVE arm).
4. **A handful of plan rows assert edits that do not exist** (taint doc
   tempering in row 1.3; Phase 0 rows were never checked off at all).

Nothing here requires re-opening the architecture. Every residual below has
a localized fix, and most are test-only.

---

## 2. Residual defects (implementation-ready, ordered by severity)

### R-1 · C-2 residual: false SATISFIED survives via escape-mediated `err` reassignment **[reproduced]** — CRITICAL class
- **Where:** `internal/static/obligations/obligations.go` — `acquireCleanLoads`
  (~:800) and the `errWebs.clean` doc comment (~:689-693).
- **What the fix got right:** direct-web pruning, the multi-store abstain via
  the reaching-definition must-analysis, the `LeakAfterReassign` regression
  (`TestReassignedErrNotSatisfied`), and preservation of the supported
  named-result + annotating-defer idiom are all correct.
- **Residual defect:** `acquireCleanLoads` observes only `*ssa.Store`
  instructions **in `fn.Blocks` with `Addr == A`**. A reassignment through a
  captured FreeVar (`run := func() { err = doWork() }; run()`) or through a
  passed pointer (`setErr(&err)`) happens in another function's blocks, so
  the load after it stays "clean" and the leaking arm is pruned. Reproduced
  at HEAD via overlay probe: both `LeakViaClosureReassign` and
  `LeakViaPointerReassign` verdict **SATISFIED** while leaking the acquire.
  This is the audit's exact bug class through a different store vehicle.
- **Comment drift:** the `clean` doc claims "a reassignment to a foreign
  value **cannot** be observed there. Safe to prune." — false for escaping
  allocs.
- **Fix shape:** evict A's loads from `clean` (→ abstain) when A is captured
  by a non-deferred `MakeClosure` or A's address escapes into any call.
  Deferred-only captures run at function exit and cannot affect mid-function
  loads, so the annotating-defer idiom survives. Fix the comment in the same
  edit.
- **Test gap (independent):** no committed test pins the CANT-PROVE arm at
  all. A mutation that deletes the abstain (keeping "explore both arms")
  leaves the whole suite green while regressing to false SATISFIED for a
  reassigned-err function whose failure arm releases.

### R-2 · M-24 defective: the quoted-identifier unwrap breaks `Normalize`'s idempotence — its own nightly fuzz gate fails in 0.11s **[reproduced]** — HIGH
- **Where:** `internal/canon/sql/sql.go:102-125` (identifier unwrap).
- **Defect:** unwrapping `"…"`/`` `…` `` emits the identifier bytes *bare*
  into `Normalized.Statement`, where a second normalization re-tokenizes
  them differently. Reproduced in a scratch checkout with the repo's own
  fuzzer: `FuzzNormalizeIdempotent` **fails after 245 execs** —
  `"0000000000000000` → once `0000000000000000` → twice `?`. Hand-verified
  variants: an unwrapped identifier containing `'` re-tokenizes as an
  unterminated literal; one containing `--` truncates the rest of the
  statement on re-normalization; `"from"` re-uppercases into the keyword.
  The canonical form is no longer a fixed point of its own normalizer — and
  the 5-minute nightly run of this fuzzer (`fuzz.yml`) will go red on its
  first execution. The PR gate stays green because it only replays seeds,
  which is why `make verify` did not catch it.
- **Fix shape:** identifiers whose bytes are not pure ident-grammar must be
  re-quoted on emit (or degraded to `?`); add the failing input as a seed.
- **Two adjacent residual gaps, same finding:**
  - **Nested block comments leak** (PostgreSQL nests `/* */`): the scanner
    stops at the first `*/`, so
    `/* outer /* inner */ secret-payload-99 */` emits
    `secret - payload - ?` into the canonical statement — a residual leak
    channel in exactly the class M-24 names. Neither existing fuzzer can see
    it.
  - **`$`-in-identifier drift:** `SELECT a$b$c FROM t` reads `$b$` as a
    dollar-quote opener and swallows the tail (Table = `""`). The
    token-boundary guard fixing exactly this was added to
    `schemadrift.stripSQL` (`migrations.go` — `isIdentByte(s[i-1])`) but not
    to `canon/sql`, while `sql.go`'s comment claims the two are "kept in
    step" — a named-parity violation with the guard on one side only.

### R-3 · C-7/M-26 residual: the `1..*` loop marker is still silently discarded when a wrapper is promoted inside a Concurrent/Unordered group **[reproduced]** — MEDIUM (M-26's own class)
- **Where:** `internal/canon/promote/promote.go` — `appendContracted`
  (~:102-105).
- **What the fix got right:** the Unordered branch, the fail-closed
  sequential single-member invariant, the sequential-splice multiplicity
  carry (`withMultiplicity`), and all demanded unit + end-to-end tests.
- **Residual defect:** when a dropped wrapper's sole child-group is promoted
  into a Concurrent/Unordered group, `members = append(members,
  cg.Members...)` flattens the child-group's members and **discards that
  child-group's own `Multiplicity`** (and, secondarily, an Unordered child
  group's flag inside a Concurrent parent — upgrading "order unknown" to an
  asserted race). Reproduced via probe: `conc(HTTP GET /a,
  wrapper(seq"1..*"(DB INSERT ledger)))` → filtered group carries
  `Multiplicity: ""` — the golden asserts the INSERT ran **once** where the
  truth is `1..*`, which is M-26's defect verbatim, surviving on the path
  the C-7 fix itself created. It also contradicts the new comment's
  "promoted into the group only when lossless": a promotion that erases a
  loop marker is lossy.
- **Fix shape:** treat a marker-carrying (or flag-carrying) sole child-group
  as NOT lossless — retain the wrapper, or carry `cg.Multiplicity` onto the
  promoted members' group. Extend the unordered promote tests with a
  `1..*` child.

### R-4 · Taint: a tainted return consumed only by a third-party caller still dies silently — package-doc promise violated **[code-verified]** — MEDIUM (new finding, C-3's family)
- **Where:** `internal/static/taint/taint.go` — `prepare` builds `callsTo`
  only from instructions in first-party functions (~:206-238);
  `handleReturn` (~:583-600) neither taints nor escapes when the only
  callers are third-party.
- **Scenario:** a type implementing `String() string { return sourceX() }`
  passed to declared sink `fmt.Println` — the invoke edge to `String` lives
  inside `fmt`, so the tainted return propagates nowhere and sets no escape
  → **NO-FLOW, escaped=false** while runtime delivers the PII to the sink.
  Contradicts the package doc's "taint passed into non-first-party code …
  is an escape" / "ANY value that escapes a modeled construct sets the
  escaped flag".
- **Fix shape:** in `handleReturn`, escape when the call graph records
  callers of `fn` not present in `callsTo` (data already in hand); or temper
  the package doc. Related comment drift: the claims at ~:195-197 and
  ~:269-271 that "a nil cg makes every invoke site an escape frontier
  (ABSTAIN)" are false for the un-seeded-source corner (with nil cg a
  source invoked only via an interface is silently un-seeded → NO-FLOW);
  exposure is API-only (the sole production caller always passes a graph).

### R-5 · M-7 partial: the hoisted boundary-label grammar is recomposed by concatenation in the schema-owner decoders, evading the repo-scan guard **[code-verified]** — MEDIUM
- **Where:** `internal/groundwork/graph/index.go:263`
  (`prefix := boundaryPrefix + "bus "`) and `:310` (`… + "db "`).
- **Defect:** the guard test scans for the quoted byte literals, so these
  concatenations pass it — in exactly the decoders (`BusEffects`,
  `DBEffects`) that feed the chains lens, the impeachment join, and
  table/event triage. A future change to the kind token in `boundarylabel`
  propagates everywhere except here, and the guard stays green while both
  decoders silently stop matching every db/bus effect. The `graph` package
  already imports `boundarylabel`; adoption is a two-line change.
- Secondary, same class: post-strip kind-token comparisons still hand-type
  `"db"`/`"bus"` in `fitness/budget.go`, `static/frontier`, and
  `static/graphio/mermaid.go` where `boundarylabel.KindDB/KindBus` exist.

### R-6 · M-2 partial: two Phase-0 sub-items remain open, deferral recorded only in a commit message — MEDIUM (trust boundary)
- **Done:** impeachsvc fixture in CI, `-race` parity, CI runs
  `make fmt-check`, lint version pinned both sides.
- **Not done:**
  - **gofmt exclusion never aligned** — `Makefile` still excludes only
    `testdata/fixtures/loansvc/`, and the exclusion predicate now exists in
    two unguarded copies (`fmt-check` target vs the inline copy in
    `verify`). Mitigated (CI calls `make fmt-check`, tree currently clean),
    but the demanded alignment did not happen and the predicate is drifting
    material.
  - **Go version split still open** — go.mod 1.24.0 / go.work 1.25.0 / CI
    1.25.11 (and the two fixtures are themselves split). Commit `010cd35`
    declares this deliberate, but no tracked doc records the decision; the
    plan row and audit still demand it. Failure scenario: code using a
    1.25-only stdlib API passes CI while violating the declared 1.24 module
    floor for downstream consumers.
  - The lint-version parity is comment-only (CI hardcodes `@v2.5.0`, the
    Makefile defaults `v2.5.0` independently) — per CLAUDE.md's own bar,
    parity needs a guard, not just prose.

### R-7 · M-11 partial: the two `reach` renders were neither converged nor renamed — LOW
- **Where:** `cmd/groundwork/main.go` (CLI `reach`: bespoke bidirectional
  callers/callees/cover/effects report) vs `cmd/groundwork/mcp.go:~896`
  (MCP `reach`: `impact.ForNodes` render).
- The triage card itself was properly unified and byte-parity-tested; but
  the audit's demand for the reach twins was "converge or explicitly
  rename", and what landed is a code comment ("DELIBERATELY a different
  view"). A user moving between the CLI and the agent surface still gets
  two different cards under one command name.

### R-8 · Minor code-level observations from verification — LOW
- **edgeOf splice depth cap** (`graphio.go:~1114-1117,1141`): on a
  pathological wrapper cycle the splice "fails closed (drops the edge)" —
  but for an absence proof a *dropped edge* is the fail-**open** direction
  (missing edge → noPathFound → PASS), and no blind spot is emitted.
  Unreachable for real go/ssa output (wrapper chains don't cycle), but the
  comment's polarity is inverted for the property that matters; consider a
  blind spot or a panic instead of a silent drop.
- **H-11 residual (outside the audit's letter, inside its spirit):** a
  writer method that *stores* `&w.sb` into a global/foreign field (an
  `ssa.Store`, not a call) while doing one visible `WriteString` still
  yields a "complete" template; a later write through the alias by another
  function mints a false constant READ. `callMayMutateReceiver` covers
  calls only.
- **M-25 note:** the shared lower-casing also applies to
  `db.collection.name`; MongoDB collections are case-sensitive, so distinct
  collections `Users`/`users` now merge under one op key. Deterministic,
  consistent with the statement path — but undisclosed.
- **M-3 note:** the capture-side flat sort still ties on span ID when two
  spans agree on start/name/kind/parent but differ only in attrs. Canonical
  output cannot inherit it (siblings and orphan heads re-sort intrinsically
  in canon), but the flat-list comment's "deterministic, run-independent
  order" is only true up to that case. The demanded fuzz-generator
  extension emits zero-goroutine children only, not the literal
  same-goroutine shape (equivalent code path; commit message overclaims).

---

## 3. Demanded tests that were not shipped

Each of these was named in the audit (or the plan's Test column) for a fix
whose code landed; the fix is unverified in the demanded direction until the
test exists.

| Finding | Missing test | Risk while absent |
|---|---|---|
| C-1 | A `must_not_reach` over the wrapper/generic seam finds the path (the audit's demanded end-to-end assertion). `TestC1SyntheticNodesRendered` pins node+edge in `Build` output, but no fitness rule crosses the seam. | The graph-side guarantee is pinned; the verdict-side one is inferred. |
| C-1 | A boundary effect **inside** a first-party generic helper reaching the write surface. loansvc's `codec.Decode[T]` carries no effect; the audit's reproduced shape 2 (`GenericSave[int] → ExecContext`) has no direct regression anywhere. | The severed-write scenario that motivated C-1's severity is only indirectly covered by node rendering. |
| C-1 (§7.2) | Dedicated "shape fixture" services were not created; the shapes were folded into loansvc instead. Acceptable as an alternative (the shapes are permanent and cross `Build`), but the two gaps above are the cost. | — |
| C-2 | The CANT-PROVE (abstain) arm — see R-1. | Silent regression path to false SATISFIED. |
| H-4 | The demanded Build-level fixture: a first-party package under `classify.db` driven through `graphio.Build`. Frame-level unit tests exist (`StringArgs(nil)` etc.), but a new `site.Common()` deref inside `Edge`'s IsDB branch or `nodeTier`'s self-edge path would panic with the unit tests green. | Reproducible-crash class regression. |
| H-2 | An "orphans are still accepted" regression — the guard against over-tightening the new completeness check exists only as an ephemeral probe from this review. | False refusal of legitimate captures. |
| H-7 | `NewCautions` rendering on the BLOCK path (PASS path is tested). | Cosmetic. |
| H-12 | Unterminated-`$$` input (behavior code-verified fail-closed, untested). | Low. |
| M-13 | End-to-end `taint --gate --strict` flag wiring (helpers are unit-tested; the four wiring lines in `cmdTaint` are not exercised). | Low. |

---

## 4. The LOW list (§5): silently skipped items

The plan marks all three §5 groups `[x]` and records exactly one deferral
(CaptureMode zero-value — whose justification **is** accurate: both
production constructors set `Mode`). Verification found the three batch
commits implemented exactly their commit-message sub-lists and left the
rest of §5 untouched, with no deferral notes. Skipped items, by audit group:

**Stale/false comments — 2 of 12 not actually fixed:**
- `internal/canonjson/canonjson.go:11-13` — the "fix" added a parenthetical
  but the flagged sentence still reads `"<uuid>" … instead of becoming
  "<uuid>"` with **byte-identical literals**; the X-instead-of-X defect the
  audit described is still there.
- `internal/groundwork/transcript/transcript.go` — `Tool()` still swallows
  the decode error into `"(unnamed)"` while `Load`'s doc claims strict
  fail-loud; the demanded Load-time validation was never added. This one is
  itself a live tenet-6 violation.

**Robustness / fail-closed — 9 skipped:**
- `opkey.go` degenerate `"RPC "` op key (falls back to nothing; distinct ops
  merge).
- `redact.go` vs `url.go` byte-identical id-shape regex duplication (no
  fold, no parity test; `url.IsID`'s "shared rule" doc claim still false).
- `fitness/exceptions.go` must_pass_through suppression attribution still
  not prefix-free across rule names (rule `a` swallows findings of rule
  `a: b`).
- `chains.go` empty consumer Name still seeds an event-`""` card; duplicate
  fleet names still last-win at the library level.
- `render.go` malformed Mermaid for an edge endpoint not in `g.Nodes`.
- `diff.go` `Diff(nil, x) == no changes` fail-open footgun.
- `golden.go` `Slug` collision still last-writes in the in-test path (the
  refusal exists only cmd-side; the demanded port never happened).
- `sqlfold` `returnsBuilderString` existential-match dependency neither
  tightened nor documented.
- `roots` interface-typed handler hint still silently no-ops (no "hint
  matched calls but never a func-typed handler" disclosure);
  `signatures.go` bare-name qualifier unchanged.
- (Config `Version` was only half-done: `< 0` rejected, but 0/future still
  accepted, so it does not "mirror policy.Validate"; mitigated by the field
  having no consumers.)

**CLI / server polish — 8 skipped:**
- flowmap **verdict-vs-operational exit-code split** — the one §5 item the
  audit sized as larger; not done, not deferred. Bare `flowmap`/`groundwork`
  still print usage and exit 0.
- `--stamp` with `--mermaid`/`--rollup`: a code comment now documents the
  intent, but the demanded user-facing warning was not added (the flag is
  still silently ignored).
- `gateEffectGoldens` still gates 0 goldens silently (no "gated N
  golden(s)" disclosure).
- Corrupt committed golden still silently overwritten on `--update`
  (`LoadEffectGolden` error ignored; no intent comment, no refusal).
- `--library-owned` still cannot override config to empty.
- `splitList`/`splitComma` duplication; `.json.gz` doc note; README layout
  line omissions — all untouched.
- (The three "when it hurts" performance suggestions — harness O(N²),
  `sort.Search`, FQN-parse consolidation — were conditional; leaving them is
  fine.)

---

## 5. Plan-integrity discrepancies

The tracking plan (`2026-07-remediation-plan.md`) misstates the record in
four places; per this repo's own standards ("the author of a change must not
be the sole grader of it"), the plan should be corrected to match reality:

1. **Phase 4's LOW rows** claim all three §5 groups landed with one
   deferral; see §4 — ~17 items were skipped silently.
2. **Row 1.3** checks off "Temper the by-source/by-sink doc comments" — no
   such edit exists in the range (`git diff 3c7fb05^..HEAD` touches none of
   those doc blocks). Materially near-moot (the fixed aggregate makes the
   comments true again, and the ABSTAIN-posture note landed in the CLI
   instead), but the row asserts an edit that never happened.
3. **Phase 0 rows (0.1–0.5) were never checked off** despite the plan's own
   "check items off as they land" instruction — and 0.1 (M-2) is in fact
   only partially landed (R-6), so a blanket `[x]` would have been wrong
   anyway.
4. **Row 0.1 / the Phase-4 wrap-up** do not record the Go-version and
   gofmt-exclusion deferrals anywhere a reader of the plan can see them
   (commit-message-only).

---

## 6. Verified resolved (no action)

Everything below was adversarially checked (fix present at the named sites,
demanded regression test present and shaped to fail pre-fix, comments
honest) and needs no follow-up beyond the notes already listed above:

- **CRITICALs:** C-1 + M-20 (predicate chain `fn.Pkg → Origin().Pkg →
  Object().Pkg()` in one home, adopted by graphio/blindspots/taint **and**
  boundary; wrappers spliced with concurrency carried; instance nodes
  rendered with origin-package attribution; FQN sort made total via
  `InstanceDiscriminator` with determinism tests) — modulo the §3 test
  gaps. C-3/C-4/C-5 (invoke-mode indexing over resolved callees, seeding,
  struct-carrier container taint, escape-on-unenumerable-invoke — all
  verdict-flipping, all PoC-shaped tests present) — modulo R-4. C-6
  (graph-wide ConcurrentDispatch + dynamic-direct probes, exact demanded
  regression). C-8 + M-1 (distinct-goroutine requirement, honest doc, exact
  regression; loud goid self-check through `TB`, tested both arms).
- **HIGHs:** H-1 (escape consumption + a genuine leak-property fuzzer in
  the nightly matrix; 2.4M execs clean during review), H-2, H-3 (skip set
  exactly matches the projection's key set; both-spellings determinism
  pinned 50×), H-5 (both-direction decoder invariant + PoC parity test),
  H-6, H-7, H-8 + M-9 (one `IsRoute`, all three sites, fallback +
  property-test roots), H-9 + H-10 (one `GateCaveats` assembly feeding
  CLI/SARIF/MCP; witness lines shared; `SQLFoldCaveat` split by via kind;
  MCP-parity tests end-to-end), H-11, H-12 (dollar-quoting with the
  ident-boundary and `$1` guards), H-13 (uniform snake_case via one
  `canonKey` fold; ambiguous double-spelling refused deterministically;
  loud goldens-present-zero-decoded gate placed before the early exit),
  H-14 (one shared vocabulary, both paths tested + parity guard).
- **Phase 0:** H-4 (guards; §3 fixture gap), M-15 (token/EOF form:
  whitespace tolerated, second value/garbage/stray delimiters rejected),
  M-33's `--update` guard (ordered before side-effects, corpus asserted
  empty).
- **MEDIUMs:** M-3 (intrinsic tie-break + Unordered fold + new fuzz target;
  capture side fixed too), M-4, M-5, M-6 (digest self-check covers every
  exported field incl. `NewCautions`; top-level per the audit's wording),
  M-8, M-10, M-12 (both resolvers fail closed on ambiguity, cross-referenced),
  M-16, M-17, M-18 (including the `6427ed1` correction of the
  first-draft always-zero count; end-to-end lost-baggage test), M-19, M-21
  (14/14 targets enumerated + a real bidirectional repo-scan parity guard
  in the PR gate), M-22, M-23, M-27, M-28, M-29 (label-set keying strictly
  stronger than the demanded pair-dedupe), M-30, M-31, M-32, M-34, M-35,
  and the M-33 remainder (clean `-h` across all nine flowmap subcommand
  forms; flag/usage parity restored with guard tests).
- **LOW items that were touched** all check out at HEAD (MCP token SHA-256
  compare + Bearer scheme + timeouts + `-32700`; policy dup-name guards for
  reach rules; `BusEffects` tally; `SortBlindSpots` total order; harness
  rand fail-loud; shared `DedupKey`; coverage guard; pin-identity
  rejection; the comment batch minus the two in §4).

## 7. Suggested follow-up order

1. **R-1 and R-2 first** — one is a live false-SATISFIED path (the audit's
   worst-outcome class), the other turns the nightly fuzz gate red the
   night it runs and un-fixes the canonical form's fixed-point property.
2. R-3 (loop-marker loss) plus the §3 missing tests — all small, mostly
   test-only.
3. R-5/R-6 (drift-guard evasion, CI parity leftovers) and the plan
   corrections in §5 — bookkeeping, but this repo's trust model runs on
   exactly that bookkeeping.
4. Triage §4: either implement the skipped LOWs or mark them deferred in
   the plan with a reason, so the tracking document stops overclaiming.
