# review-fixes: implementation plan

**Status:** plan-of-record for fixing the 16 confirmed findings from the
branch-wide code review (10 ranked + 6 cut by the output cap). Organized by
**root cause**, not by finding: several findings that look unrelated are one
defect surfacing in multiple places, and fixing the cause retires them
together. Each phase carries its regression locks.

---

## 1. Root-cause taxonomy

| # | Root cause | Findings it explains |
|---|---|---|
| RC-1 | **Open-world inputs handled with closed-world defaults** — switches/matchers over externally-supplied vocabularies (graph statuses, policy selectors, CLI flags) treat "unrecognized" as "absent", violating the framework's own *silence-is-never-a-silent-pass* discipline | unknown obligation status judged like SATISFIED; `entrypoint:*` inert in `must_not_reach`; second triage flag silently ignored |
| RC-2 | **Identity proxies instead of canonical identity** — comparing counts where sets are meant, multisets where sets are meant, empty strings where unique keys are meant | exceptions false-DEAD (count equality); duplicate `no_concurrent_reach` findings; invalid-Pos sites collapsing finding identity |
| RC-3 | **The abstention predicate lags the artifact's content model** — `NO-STRUCTURAL-SIGNAL` is defined on the node/edge delta, but artifact content now derives from graph sections (obligations, blind spots) that change without a structural delta | body-only change with a new CANT-PROVE caution renders as "the graph has nothing to say" |
| RC-4 | **SSA facts recognized by narrow syntactic pattern instead of semantic query** — direct-Extract-only error tracking, `t.String()=="error"`, one-level `AnonFuncs`, `*ssa.Call`-only site collection, capture-Store conflated with escape-Store | false VIOLATED on single-result / captured-err / concrete-error-type acquires; closure-rollback CANT-PROVE with misleading reason; deferred `Before` invisible → false UNMATCHED; `usesRecover` missing `defer handle()` and nested closures |
| RC-5 | **Partial normalization at an output boundary** — `site()` falls back to machine-specific absolute paths exactly where normalization fails; entry-scoped builds reuse whole-service semantics (`UNMATCHED` = "inert in the analyzed unit") on a unit that isn't the service | absolute paths in graph.json for out-of-dir files; scoped builds emitting phantom UNMATCHED / dropping real VIOLATED |
| RC-6 | **Per-subcommand reinvention of CLI and reporting conventions** — flag handling and summary lines hand-rolled per command instead of using the house mechanisms | `exceptions` usage documents an invocation `flag.Parse` rejects; "fitness OK — 0 invariant(s)" while obligations were judged; cautions print no Detail; `ResolveFrame`'s dead disjunct (an unstated matching contract) |
| RC-7 | **Rule-independent facts recomputed inside per-rule/per-item loops** — and one brute-force differential (Exceptions re-running the full Check per allow entry) whose *mechanism* is also what made RC-2's count proxy possible | the whole efficiency cluster: concurrent per-rule rescans, Exceptions O(entries × full-check), impact per-suspect BFS + double Effects, reachExisting per-site BFS, obligations O(rules × instructions), passthrough's post-answer blind probes |

Note the two deliberate cross-links: fixing RC-7's Exceptions mechanism *is*
the fix for RC-2's count proxy (the differential disappears entirely), and
RC-2's canonical-key work supplies the primitive RC-7's set-difference needs.

## 2. Fix design, by phase

Phases are ordered so shared primitives land before their consumers. One
commit per phase; every phase starts by committing the **failing reproduction
tests** the finders supplied, then the fix.

### RF-1 — fail-closed interpretation boundaries (RC-1)

- **Judging:** `fitness.checkObligations` gains a `default:` arm — an
  unrecognized status emits a Caution: *"graph emitted obligation status %q
  this groundwork does not understand — upgrade or investigate"*. The same
  convention is documented in `graph.Obligation`'s comment as the contract for
  every future graph-carried vocabulary: **unknown enum → disclosed caution,
  never fall-through.**
- **Selectors:** selector expansion is promoted from a passthrough-private
  helper to the shared matching layer: `fitness.expandFroms(ix, patterns)`
  (Sources for `entrypoint:*`, `matchNodes` for the rest), used by
  `checkMustNotReach` *and* `checkMustPassThrough` — one selector language for
  every From position. `policy.Validate` rejects `entrypoint:*` in any
  non-From position (To, Through), so an unsupported placement is a load
  error, not a silent no-match.
- **CLI exclusivity:** `cmdTriage` counts the symptom flags and errors unless
  exactly one is set.

*Regression locks:* unknown-status → caution test; `must_not_reach` with
`entrypoint:*` now produces findings on a fixture where an entrypoint reaches
the target (and `require_proof` fires on the blind fixture); validation
rejects `to: ["entrypoint:*"]`; two-flag triage invocation errors. Existing
selector tests unchanged.

### RF-2 — canonical identity and sets (RC-2, plus the Exceptions half of RC-7)

- **One finding key.** `review.findingKey` moves to
  `fitness.(Finding).Key()` (Rule, From, To, Summary — Detail excluded), and
  review imports it. This is the single definition of finding identity the
  base-vs-branch diff, the Exceptions attribution, and any future consumer
  share.
- **Exceptions: suppressed-set attribution replaces the N+1 differential.**
  Run `Check` twice: once as configured (baseline), once with every audited
  allow-list emptied. `suppressed = findings(empty) − findings(baseline)` by
  key. An entry is **LIVE iff it matches at least one suppressed finding**
  (layering entries match the finding's From/To via the same exception
  matcher the check uses; pass-through entries via `rule.Allowed(from, to)`;
  blind-spot entries keep the present-spot match). This fixes the
  caution↔violation count-swap false-DEAD *by construction* — there is no
  count to fool — and replaces O(entries × full-check) with exactly two
  checks.
- **Concurrent findings become a set.** `checkNoConcurrentReach` is rebuilt
  on deduplicated primitives: seeds and cone as `setutil` sets, and one
  report path — collect violating (from, target) pairs into a keyed set
  first, emit once each with a single summary form. The direct-edge loop and
  the Effects loop can no longer double-report one edge because they feed the
  same set.
- **Site identity never collapses.** Covered in RF-5 (invalid-Pos fallback).

*Regression locks:* the exact swap scenario (one allow-listed bypass + blind
cone) asserts LIVE; an entry excusing nothing asserts DEAD (existing tests);
double-spawn fixture asserts exactly one finding; attribution-vs-differential
equivalence asserted on every existing exceptions test (same verdicts,
new mechanism).

### RF-3 — abstention follows the artifact, not the delta (RC-3)

`verdict()`'s abstention condition becomes a statement about the artifact's
whole signal, so any future finding source is covered automatically:

```go
hasSignal := !d.empty() ||
    len(newViolations)+len(newCautions)+len(newBlindSpots) > 0
// NoStructuralSignal iff !hasSignal; Block conditions unchanged.
```

For truly identical inputs nothing changes (identical graphs ⇒ identical
findings ⇒ no new anything ⇒ still NO-STRUCTURAL-SIGNAL), so the existing
abstention tests stand. A body-only change that surfaces a new obligation
caution now renders STRUCTURALLY-CLEAR with the caution section visible.

*Regression locks:* reproduction test (identical nodes/edges + one new
CANT-PROVE → not NSS, caution rendered); `TestReviewNoStructuralSignal`
unchanged; artifact goldens regenerated only if any fixture's verdict
actually changes (none should — obligsvc review tests construct their own
branches).

### RF-4 — semantic SSA queries (RC-4); the largest phase

One new shared primitive retires three findings at once:

- **A local value web.** `valueWeb(root)` computes the values aliasing a root
  within one function: tuple Extracts, Phis, conversions/interface boxing,
  **Store-to-local-Alloc → loads from that Alloc** (the captured/named-var
  case SSA doesn't lift), transitively. Both error tracking and resource
  tracking consume it:
  - `acquireErrValues` → `errorValuesOf(acq)`: the web of every acquire
    result whose type **`types.Implements` the `error` interface** (fixes
    single-result acquires — the call value itself is the err — concrete
    error types, and captured/Phi-routed err in one mechanism).
  - `resourceValues` uses the same Implements test to exclude error
    components, and `ownershipEscapes` walks the web: a Store *into a local
    Alloc* is alias propagation, not escape; a Store anywhere else (heap
    object, global, through a parameter) remains escape.
- **Deferred-closure release credit.** A `MakeClosure` over the resource
  whose **only use is a `Defer` in the same function** is not an escape; and
  if the closure's body contains a release-matching call, that Defer is
  credited as a defer-of-release in `leakPath`. This turns the dominant
  idiom `defer func() { _ = tx.Rollback() }()` from CANT-PROVE-"stored" into
  SATISFIED. A closure with any other use (passed, stored, returned) stays an
  escape.
- **Deferred/spawned Before sites count.** `checkPrecede` collects
  `ssa.CallInstruction` (Call, Defer, Go) for the **Before** anchor, using
  the *registration/spawn site* as the B site — sound (an A dominating the
  registration executes before the deferred B can run) and never under-strict.
  Require sites remain plain Calls only (the existing comment's reasoning
  holds for A). This also retires the false-UNMATCHED, since the deferred
  publish is now a matched site.
- **`usesRecover` becomes defer-rooted.** recover affects `fn` iff a function
  *deferred by fn* calls recover directly. Implementation: for each Defer in
  fn, resolve the deferred function (StaticCallee or MakeClosure target) and
  scan it for a direct recover call. This catches `defer handlePanic()` and
  arbitrary closure nesting at the defer root, and stops flagging recovers in
  non-deferred synchronous closures (the current scan is simultaneously
  under- and over-broad). **Disclosed residual:** a *dynamic* deferred func
  value (e.g. `defer cancel()`) cannot be resolved; abstaining there would
  abstain on most real Go, so it is accepted and documented in the package
  doc beside the implicit-panic exclusion — same honesty mechanism, same
  rationale.

*Regression locks:* every empirical shape from the review becomes a permanent
fixture — unit-table rows AND obligsvc functions (single-result acquire,
err-annotating named-result defer, `*TxError` signature, closure rollback,
`defer handle()` recover, `defer publish(...)`) with their **expected
post-fix verdicts**: the first three flip false-VIOLATED/CANT-PROVE →
SATISFIED-or-pruned, closure rollback → SATISFIED, named recover →
CANT-PROVE, deferred publish → VIOLATED-without-audit. The obligsvc golden
regenerates **in this phase's commit with the diff enumerated in the commit
message**; all other goldens must remain byte-identical (the zero-impact
guarantee re-asserted).

### RF-5 — total normalization and scope honesty (RC-5)

- **`site()` never emits machine-specific output.** Fallback ladder:
  module-relative path (current case) → for files outside baseDir, a
  **portable package-qualified form** `<pkg-import-path>/<file base>:<line>`
  (stable across checkouts, still navigable) → for an invalid Pos, a
  **synthetic-but-unique** form `<file-or-pkg>:synthetic#<ordinal>` (ordinal
  of the site within the function), so identity never collapses.
- **Entry-scoped builds emit no obligations section.** Obligations are a
  whole-service disclosure (a level-2 slice of the *full* graph); evaluating
  them over an entry cone makes UNMATCHED a scoping artifact. `graphio.Build`
  computes them only when `entry == ""`, with a comment stating why — the
  cheapest honest semantics, revisited only if a consumer ever needs scoped
  verdicts.

*Regression locks:* unit tests drive `site()` through all three ladder rungs
(synthetic positions constructed directly); the cross-checkout byte-identity
test stays; a new graphio test asserts an entry-scoped build of obligsvc
contains no `obligations` key while the unscoped build does.

### RF-6 — one CLI and reporting convention (RC-6)

- **Flag placement:** `triage` and `exceptions` adopt the same mechanism
  `review`/`verify` already use to accept trailing flags (the existing house
  helper — reuse it, don't write a third). Usage strings then match actual
  accepted syntax everywhere; a CLI test runs each documented form verbatim.
- **Fitness summary tells the truth:** the OK line reports what `Check`
  actually evaluated — policy rule count *plus* `N obligation verdict(s)
  judged` when the graph carries them — and a single finding-render helper
  prints Detail for violations *and* cautions (removing the copy-paste pair
  that caused the asymmetry).
- **`ResolveFrame` gets a stated matching contract:** exact FQN → frame-form
  conversion → **token-bounded suffix** (preceded by `.`, `(`, `*`, or
  start-of-string). The dead disjunct disappears because the contract is
  implemented once; `--frame User` no longer matches `GetUser`.

*Regression locks:* documented-invocation CLI tests; suffix boundary table
(`User` ∉ `GetUser`, `GetUser` ∈ `(*pkg.T).GetUser`, exact FQN unchanged);
fitness summary asserted on an obligations-only fixture.

### RF-7 — hoist rule-independent computation (rest of RC-7)

All behavior-preserving; the committed goldens and existing tests are the
oracle that output is identical.

- `checkNoConcurrentReach`: edge scan, cone, effects, and blind probe hoisted
  above the rules loop (falls out of RF-2's rewrite — verify it landed).
- `checkMustPassThrough`: skip `frontierBlindSite` once `blind` is true and
  feed it the already-computed effects; node-scan amortization across rules
  deferred until profiled (don't speculate).
- `impact.ForNodes`: one multi-seed `Reaching` + one `Sources()` intersection
  replaces per-suspect `EntrypointCover`; effects gathered once and reused by
  the dynamic-blind-spot pass.
- `review.reachExisting`: one multi-seed reverse BFS + one Sources scan.
- `obligations.Check`: one instruction pass per function bucketing call sites
  by (pkg, name) consumed by all rules; `usesRecover` memoized per function.

*Regression locks:* no golden may change in this phase — `git diff
testdata/` must be empty after `regen.sh`; all fixture-exactness tests
(hand-derived sets) unchanged. Any divergence is a bug in the hoist, by
definition.

## 3. Cross-cutting regression protocol

1. **Reproduce first.** Each finding's failing test is committed in the same
   phase as its fix, named for the behavior (not the review), so the suite
   documents the contract forever.
2. **Golden discipline.** Only RF-4 may change a committed golden, and only
   obligsvc's; every phase ends with `regen.sh` + `git diff testdata/` to
   prove the zero-impact guarantee for everything else. Artifact digests are
   expected to change only where verdicts legitimately change, enumerated in
   the commit message.
3. **Determinism re-checks per phase:** the cross-checkout byte-identity test
   and `TestReviewDeterministic`/`TestRatchetDeterministic` run in every
   phase (they are in the suite; the protocol is simply that no phase may
   skip the full suite).
4. **Equivalence assertions for mechanism swaps:** RF-2's Exceptions rewrite
   keeps every existing liveness test and adds the swap case — the old tests
   pin the verdicts the new mechanism must reproduce; RF-7 is pinned by the
   goldens.
5. **Order matters:** RF-1..3 are independent and small; RF-2 lands before
   RF-7 (shared key primitive); RF-4 is the bulk and sits in the middle so
   its golden churn doesn't interleave with mechanical phases; RF-5..7 close.

## 4. What is deliberately NOT being fixed (disclosed residuals)

- **Version-skew decode failures** (old groundwork ↔ new graph/artifact
  fields): the strict decoder failing loudly on unknown fields *is the
  documented design* — lockstep deployment is the contract, and softening it
  would reintroduce silent field-dropping. Residual accepted; the operational
  note belongs in release/upgrade docs, not code.
- **Render text drift** (trailing newline, `via` lines): presentation is
  uncommitted by design; the digest covers canonical JSON only.
- **Gate vs Review blind-spot asymmetry**: deliberate (everything a
  GateResult lists is a reason it failed); documented at the field.
- **Dynamic deferred func values in `usesRecover`** (RF-4): accepted and
  documented; abstaining there would abstain on `defer cancel()` everywhere.
