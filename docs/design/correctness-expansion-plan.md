# correctness expansion: implementation plan

**Status:** in progress — CX-0 (the summary engine), CX-2 (the must-precede
lifts, `fromCallers`-gated per D-CX9), CX-3 (derived effect sites with
`via` provenance), CX-1 (the must-release handoff credit), and CX-4 (the
sensitive-flow rule pack in usage.md, with its locking test) shipped. Only
CX-5 remains, parked on the adopter gate (E-CX5) and the field measurement
run (E-CX6).

The §10 adversarial review ran before CX-2 merged and found four issues — F1
(a conditionally-releasing deferred closure earned ALWAYS through
`deferReleases`' any-instruction scan), F2 (entry domination ignored
unresolved invoke dispatch), F3 (the entry NEVER pole was not a proof; ED is
now two-valued and per-edge dominance became a coverage walk), F4 (the
`Unit.Callees` contract permitted unsound pre-filtering) — all fixed with
locked reproductions, mirroring the v1 obligations review. CX-1's walk
splits in two — a leak hunt (an unknown handoff blocks the witness) and a
proof hunt (an unknown handoff is transparent, so a later unconditional
release still proves) — so the VIOLATED witness is never weaker than the
intraprocedural one and an early maybe-release cannot force a false
abstention.

A post-batch /code-review pass found one confirmed regression — CX-3's
derived sites removed their carrier calls from OrderFacts' fault-site list,
silently deleting loansvc's two pre-existing facts, and the wholesale golden
regen ratified the loss — fixed (direct-site-only exclusion + self-pair
skip) and locked with a semantic fact assertion regen cannot launder
(TestEffectOrderKeepsCarrierFaultSites). The review's efficiency findings
are recorded follow-ons, deliberately deferred to the E-CX6 wall-clock
measurement: per-label cone rescans in never(), the eager whole-program
condensation on first rule (lazy/scoped condensation is the prepared fix),
merging the three full-universe sweeps (computeSCC / entries /
addressTaken), a typed summary key replacing the "\x00effect:" namespace
packing, an engine-owned program-adapter constructor enforcing the F4
preconditions, and unifying the three CFG coverage walks. Companion to
[`path-obligations-plan.md`](path-obligations-plan.md) (whose §4/§10
"no interprocedural" limit D-CX2 supersedes, for *proven* summaries only) and
[`guardrail-extensions-plan.md`](guardrail-extensions-plan.md) (whose §1
extension recipe governs every check here). Grounded in the actual code (file
references verified), not in aspiration.

**Revised 2026-06-12** against the first field measurement run (a three-service
deployment, CI-equivalent graphs @ `9ae5b15`, numbers measured not estimated).
The evidence reshaped the build order (D-CX8) and CX-2's shape (D-CX7); field
figures are tagged **[field]** below, and §2b carries the run's findings.

This plan widens the one claim the framework already makes — *universal,
all-paths structural proof* — across call boundaries and, eventually, service
boundaries. It deliberately does **not** cross into value/logic correctness;
§1 fixes that boundary before anything is built.

Scope decisions made when this plan was cut:

- **D-CX1 — interprocedural reasoning is summary-based and three-valued.**
  Per (rule, function) summaries — ALWAYS / NEVER / UNKNOWN — computed
  bottom-up over the existing call graph, memoized in reverse topological
  order, SCCs abstaining. No fixed-point iteration, no widening: the summary
  is a pure function of (SSA, call graph, rules), byte-deterministic like
  everything else.
- **D-CX2 — the trust-monotonicity invariant.** In this slice,
  interprocedural reasoning may only (a) upgrade VIOLATED → SATISFIED by
  *proving* the obligation is met in a callee, or (b) downgrade
  VIOLATED → CANT-PROVE with a disclosed reason. It must **never** introduce a
  VIOLATED that the intraprocedural analysis did not already report. The gate
  can only get less noisy, never more. This supersedes the
  "rule vocabulary is the mechanism" clause
  (path-obligations-plan §4, `obligations.go:20-26`) for callees the summary
  *proves*; naming the helper as a release ref remains valid, remains the
  escape hatch for UNKNOWN summaries, and remains the only mechanism for
  callees outside the analyzed unit.
- **D-CX3 — interprocedural questions are asked only at handoff sites.** A
  callee's summary is consulted only where the tracked resource's value web
  (the alias machinery that already exists — `obligations.go:276`) visibly
  flows into the call as receiver or argument. Calls that never touch the
  resource keep today's semantics exactly. This bounds both the credit and
  the abstention to the resource's actual handoff points; it reuses an
  existing deterministic structure and introduces no value semantics.
- **D-CX4 — no value-level taint in this plan.** "PII never reaches a log
  sink" and "untrusted input never reaches raw SQL unsanitized" are
  expressible *today* as `must_not_reach` / `must_pass_through` rules, with
  exactly those rules' soundness story (absence-of-path is proof modulo
  disclosed blind spots; presence-of-path is a lead, not a proven flow).
  CX-4 ships vocabulary and fixtures for that framing — **no new engine**.
  Argument-level taint ("*this parameter's* data reaches the sink") is value
  semantics; it fails the §1 acceptance criterion and stays shelved per the
  standing decision in
  [`distilled-learnings.md`](../groundwork/distilled-learnings.md) ("revisit
  only if routing bugs slip past tests in practice"). The shelving trigger is
  named there; this plan does not re-litigate it.
- **D-CX5 — cross-service ordering is observational first.** Per-service
  facts are proven; the cross-service join is declarative (event-name match,
  as fleet-events already does) and rests on broker semantics no code
  analysis can prove. So broker assumptions are **declared in policy, never
  inferred**, every chain card states them, and the surface ships
  non-gating — the post-hoc-ingestion discipline (observe first, gate on
  evidence), same as GX-2's rollout.
- **D-CX6 — zero graph.json schema change for CX-1/2.** Findings ride the
  `obligations[]` open-kind envelope (D-OB5) with unchanged kinds and
  unchanged identity keys (D-OB6: site, never prose); only verdicts and
  `detail` text move. CX-3 extends the existing `effect_order` section with
  derived sites — additive, lockstep-regenerated. CX-5 is a groundwork-side
  rendering over artifacts that already exist.
- **D-CX7 — must-precede lifts in BOTH directions, both monotone.** The
  bottom-up derived-A lift (a callee that ALWAYS calls Require counts as an A
  site) covers the wrapped-audit case; the field run exposed its inverse as
  the *real-world* false positive — the A in the **caller**, the B in the
  **callee** (`validate-before-publish` VIOLATED at both publish sites though
  validation provably runs one frame up). That case needs a top-down **entry
  domination** summary (§5). Both lifts obey D-CX2: upgrade-or-abstain, never
  a new VIOLATED.
- **D-CX9 — the entry-domination lift is opt-in per rule (`fromCallers`),
  because must-precede carries two intents the lift must not conflate.**
  Building the fixture exposed it: the obligsvc `main` exercises every shape,
  so `main`'s early call to the always-auditing `Disburse` entry-dominated
  `DisburseRacy` — the canonical VIOLATED would have flipped SATISFIED off an
  *incidental upstream require*. That proof is real for the letter of the
  lifted semantics and wrong for the rule's intent. The split: **guard**
  intent (auth, validation — the field's `validate-before-publish`) wants
  chain domination, exactly what the lift proves; **pairing** intent (an
  audit per publish, outbox ordering) wants operation-scoped domination,
  which whole-chain entry domination guts. So the rule author declares the
  intent: `fromCallers: true` opts a require/before rule into the lift;
  default off preserves today's semantics exactly. Derived-A recognition is
  unconditional for every rule — it widens which calls count as A sites
  *within the same function*, not the rule's scope. Two engine soundness
  lessons land with it, both caught by fixtures before any field run: the
  summary universe must be the whole built program, not the call graph's
  node set, because package initializers run before main and can take a
  function's address or call into the service without being RTA-rooted; and
  the entry-domination guard is only sound because address-taking anywhere
  in that universe forces abstention.
- **D-CX8 — build order follows the field evidence (supersedes this plan's
  first-draft order, the D-GX1 precedent).** CX-0 → CX-2 → CX-3 → CX-1 →
  CX-5; CX-4 in parallel, **on clean graphs only** (§7). Rationale, all
  measured: zero demand for CX-1's handoff credit (the field tx idiom proves
  intraprocedurally — 2 SATISFIED / 0 CANT-PROVE, because acquire and release
  are co-located in `RunInTx`); a live false-VIOLATED demanding CX-2's
  top-down lift; every distinct effect_order fact in the field sitting in a
  helper below its handler (CX-3's same-function miss, by construction); and
  the field's outbox invariant needing CX-3's ALWAYS-effect summaries —
  making CX-5 *dependent*, not parallel.

---

## 1. The fault line, fixed before anything is built

Correctness splits in two, and only one half is admissible here:

- **Value-blind / all-paths** — properties of the call graph + CFG that need
  no runtime values: "every tx path commits or rolls back", "the audit write
  precedes the publish", "no unauthenticated path reaches the charge API".
  Decidable-ish; a SATISFIED is a universal proof no test suite can produce.
  This is the lane the framework already occupies, and the lane this plan
  deepens.
- **Value / logic** — "is the amount right?", "is the SQL predicate right?",
  "does it handle the empty list?". Undecidable in general (Rice), and any
  spec of "right" has to come from somewhere — tests are that spec, by
  example. Approximating it would put heuristics in the verdict path and
  betray the property the whole framework is built on.

The §1 acceptance criterion from the extension recipe applies verbatim: every
check below is a pure function with a sound abstention, or it does not ship.
The framework's existing answer to the value half is already built and stays
the answer: `NO-STRUCTURAL-SIGNAL` says "this is exactly where logic review
and tests matter" (`internal/groundwork/review/artifact.go:40`), and
`flowmap coverage` points tests at the effects nothing exercises
(`internal/coverage/coverage.go`). This plan targets testing's blind spot
(the unexercised path); it never competes with testing on values.

## 2. The interface (verified facts)

- The must-release walk treats **any** call matching a release ref as
  covering — it is value-blind about *what* is released
  (`obligations.go:417-419`). Summaries inherit this: "g always releases"
  means "every path through g calls a release ref", the same claim inlining
  would have produced.
- The value web already aliases the acquired resource through extracts, phis,
  conversions, and local-slot round-trips (`obligations.go:276-312`), and
  argument passing is deliberately not an escape (`obligations.go:314-321`).
  D-CX3's handoff detection is a membership test against this existing
  structure — no new alias machinery.
- `deferReleases` documents its own ceiling: a deferred **anonymous** closure
  is scanned one level, but a deferred **named** helper must be listed as a
  release ref (`obligations.go:464-486`). CX-1 lifts exactly this with the
  same summaries.
- Every call-graph node carries its `*ssa.Function`
  (`internal/static/callgraph/callgraph.go`, `Node{FQN, Func, Out, In}`), so
  bottom-up summary computation walks a structure that already exists.
  Call-graph reachability is over-approximate (RTA), which makes
  NEVER-summaries *sound*: if no release ref is reachable in the
  over-approximated cone, none is reachable in reality.
- `effect_order` is same-function only, and the scorecard discloses it on
  every fault card ("absence is never an all-clear",
  `effectorder.go`, scorecard "Partial-effect facts"). CX-3 extends it one
  proven level at a time.
- The scorecard's standing residuals bind this plan: the obligations SSA
  analysis is flagged as the bus-factor risk (six semantic bugs found by
  adversarial review in v1) — so CX-0/1/2 budget the same adversarial review
  pass before merge, and every reviewed idiom lands as a locked unit table.

## 2b. Field evidence (measurement run, 2026-06-12)

A real deployment (three services; the dense one at 891 nodes / 4227 edges /
107 blind spots, the clean ones at ~100–140 nodes / 0 blind spots) answered
the plan's empirical questions from CI-equivalent graphs before any CX code
exists. What the numbers established:

- **The abstention fear was wrong; the false-positive fear was wrong-shaped.**
  A trial obligation run returned 2 SATISFIED / 2 VIOLATED / **0 CANT-PROVE**.
  Interface density does not bite an intraprocedural obligation when acquire
  and release are co-located (the `RunInTx` runner idiom: the interface and
  the closure sit *below* the release, never between acquire and exit). The
  measured cost is a **must-precede false-VIOLATED across a one-frame split**:
  the require (`ValidatePayload`) in the caller, the publish in the callee.
  That single finding re-anchors CX-2 (D-CX7) and demotes CX-1 (D-CX8).
- **CX-3's limit binds by construction, not occasionally.** The field's 29
  effect_order facts are 5 distinct facts path-multiplied, and **every one
  lives in a helper below its handler** — a fault card on the handler sees
  none of them today. The field-drafted incident scenario (a non-transactional
  dual fan-out: first topic published, fault before the second — "what
  already happened?") becomes a locked fixture shape (§9).
- **CX-4 splits by graph, not by rule.** The dense service is the *wrong*
  taint home (no PII, 84 log sinks, 107 HighFanOut — constant noise); the
  clean service is the *right* one (PII concentrated in ~3 files on a
  0-blind-spot graph, with one file holding both PII and sinks). And no
  scrubber/sanitizer functions exist anywhere — there are no waypoints to
  require. §7 turns both facts into shipping preconditions.
- **The outbox invariant is a forcing function.** Its ordering half is
  expressible today; its atomicity half (both DELETEs in one committed
  transaction, both-or-neither) decomposes onto CX-3's ALWAYS-effect
  summaries plus an existing rule family (§8) — which is what reordered the
  build (D-CX8).
- **The broker declaration has a real signature target:** at-least-once,
  unordered, idempotent consumers (inbox dedup) — exactly the D-CX5 policy
  shape, asserting only what the system guarantees.
- **The O-CX2 commitment is live:** the deployment will run the
  summaries-disabled vs. -enabled verdict diff on its CI graphs once CX has a
  prototype and return the deltas — trust monotonicity and the abstention
  budget measured where it counts.

## 3. CX-0 — the summary engine (`internal/static/obligations/summaries.go`)

For each rule and each first-party function with a body, a three-valued
summary answering "does this function discharge the obligation itself?":

- **ALWAYS** — every path from entry to every exit passes a call matching the
  target refs (release refs for must-release, the require ref for
  must-precede, a committed effect for CX-3) — the existing forward-walk
  machinery, run with the function's entry as the start node. Plain calls and
  defers count exactly as they do intraprocedurally. A function whose own
  proof depends on a callee consults that callee's summary — bottom-up
  composition, memoized.
- **NEVER** — no matching call (static or invoke-mode, the existing
  `ref.matchesCall` semantics) is reachable in the function's transitive
  call-graph cone, **and** the cone touches no blind spot. Sound under RTA
  over-approximation; cheap (a reachability query over the graph that already
  exists).
- **UNKNOWN** — everything else: matching calls on some paths only, recursion
  (any SCC member), `recover`, an unresolved dynamic frontier in the cone, or
  a body the unit cannot see. UNKNOWN is never silently treated as either
  pole.

One **top-down** summary joins the family (D-CX7): **ENTRY-DOMINATED(fn,
rule)** — "has the Require already executed on every entry into fn?" It holds
iff fn is not itself a graph source, no unresolved (blind-spot) edge targets
it, and *every* call edge into fn is either dominated in its caller by an A
site (derived-A included) or originates in a caller that is itself
ENTRY-DOMINATED. Computed over the same SCC condensation in topological order
(SCC membership ⇒ UNKNOWN), memoized, byte-stable like its bottom-up
siblings.

**Determinism.** Summaries are computed in reverse topological order over the
condensation (SCC-collapsed) graph, which is itself derived from the
already-sorted node and edge order; SCC membership ⇒ UNKNOWN, so no iteration
order can influence a result. A summary table for a fixed (graph, rules) input
is byte-stable, covered by the same cross-checkout test discipline as sites.

**Cost.** One walk per (rule, function-with-relevant-cone); functions whose
cone is NEVER short-circuit on the reachability query without a CFG walk —
the common case, so rule-free and rule-irrelevant code stays near-free,
preserving the obligations engine's existing cost profile.

## 4. CX-1 — interprocedural must-release credit

`leakPath` changes at exactly one instruction class: a call (or defer) at a
**handoff site** (D-CX3 — a resource-web value among the call's operands)
consults the callee's summary:

| callee summary | walk behavior | verdict effect |
|---|---|---|
| ALWAYS | covered from this point — identical to an inline release | false VIOLATED → SATISFIED |
| NEVER | not a release; walk continues | today's verdict, now backed by a stronger claim |
| UNKNOWN | the acquire site verdicts CANT-PROVE: *"release may occur inside `<fqn>` (releases on some paths / unresolved); beyond proof — name it as a release ref to assert it"* | VIOLATED → CANT-PROVE (disclosed) |

Non-handoff calls are untouched. Deferred **named** helpers with ALWAYS
summaries now cover (lifting the documented `deferReleases` ceiling); the
anonymous-closure scan stays as-is. Escape analysis (`ownershipEscapes`) is
unchanged — returned/stored/goroutine-handed resources still abstain before
any walk happens.

The D-OB1 worked example is preserved by construction: `debit(tx, …)` where
`debit` never reaches a release ref is a NEVER handoff — still VIOLATED, same
witness. The known risk is the UNKNOWN row: in store-heavy code, a handoff
callee whose cone *can* reach a release converts a crisp VIOLATED into a
CANT-PROVE. That trade is deliberate (claiming "a path exists where it fails"
is no longer a claim we can back), and E-CX2 measures whether it stays cheap.

**[field] Demand check:** the measured deployment has *zero* need for this
credit — its sole production tx idiom (`RunInTx`) co-locates acquire and
release, proving SATISFIED intraprocedurally with no abstention. CX-1 is
retained because the bug class is general (and it lifts the documented
`deferReleases` named-helper ceiling), but it builds *after* CX-2/CX-3, which
have measured demand (D-CX8). If the first adopters' idioms keep proving
intraprocedurally, E-CX1's kill clause applies to this phase specifically.

## 5. CX-2 — interprocedural must-precede (both directions, D-CX7)

The field run handed this slice its worked example: `validate-before-publish`
(`ValidatePayload` must precede `Publish`) reports VIOLATED at both publish
sites in `publishWithFanout` — yet validation provably occurs, one frame up
in `doPublish`. The B sites are real; the A lives in the caller; the
intraprocedural scope turns a *satisfied* invariant into a standing false
positive a reviewer would learn to ignore. Two lifts, each sound, each unable
to mint a new VIOLATED:

- **Derived A (bottom-up).** A plain call to a callee whose summary is
  ALWAYS-calls-Require counts as an A site: if it dominates a B, every path
  genuinely executed the require first. Flips the wrapped-audit
  false-VIOLATED.
- **Entry domination (top-down).** For a B site with no dominating A in its
  own function (and a rule opted in via `fromCallers`, D-CX9), consult
  ENTRY-DOMINATED(fn) (§3): every entry into fn has the require behind it ⇒
  SATISFIED, with a witness entry in `detail`; anything unprovable ⇒
  CANT-PROVE with the reason. *Amended by the adversarial review (F3):* the
  draft's third outcome — "a proven A-less entry keeps VIOLATED with a
  sharper witness" — was not a proof (per-site non-domination is not
  avoidability, and graph sources are not provably require-less: package
  initializers and out-of-unit callers exist), so ENTRY-DOMINATED has **no
  NEVER pole**: the consumer keeps its own intraprocedural VIOLATED rather
  than borrowing a witness the engine cannot back. The same review replaced
  per-edge *dominance* with a **coverage walk** (every caller-entry→site path
  passes an A) — strictly more precise: two requires on the arms of a branch
  cover the join without either dominating it. Verdict mapping is monotone by
  construction (D-CX2): the lift can only confirm or abstain legibly.

The **B-side detection stays intraprocedural** in this slice, disclosed in
the kind's doc comment: deriving B sites from callees that *may* reach a B
would mint new VIOLATED findings from over-approximated cones — exactly what
D-CX2 forbids. A publish hidden inside a helper escapes a rule *anchored in
the caller* in v1 (anchoring the rule on the helper's own function — where
the field rule naturally binds — does not have this gap); lifting it
(ALWAYS-calls-B derivation, sound but partial) is a named follow-on, not a
silent gap.

## 6. CX-3 — effect_order through calls

The same ALWAYS machinery, pointed at committed effects: if helper `g`
performs `boundary:bus PUBLISH loan.approved` on **every** path, then a call
to `g` in `fn` is a **derived effect site** in `fn`, and `OrderFacts` runs
over it unchanged. Derived sites carry the callee FQN in a `via` field
(presentation, additive schema, lockstep-regenerated with goldens).

ALWAYS-only derivation keeps the facts true by construction: triage's
partial-effect answers ("the publish had already happened when the charge
faulted") get strictly more coverage and zero wrong rows. MAY-effects are not
derived — an existential fact built on an over-approximated cone would put a
maybe into a fault card that responders treat as ground truth.

*Implementation note (observed on the fixture):* every derived row is true,
but volume concentrates in orchestrator-shaped functions — the fixture's
sequential `main` accrues a derived site per ALWAYS-effect callee, multiplied
by its later fallible calls. Real handlers have few callees, so the bet is
that fact counts stay proportionate outside composition roots; the E-CX6
field run measures exactly this, and a scoping knob (e.g. exclude graph
sources from derivation) is the prepared fallback, deterministic either way.

This phase is what makes CX-5 possible: cross-service chains compose
*proven* per-service ordering facts, and same-function-only facts are too
sparse to compose.

**[field] The limit binds universally, not occasionally:** all five distinct
effect_order facts in the measured deployment live in helpers below their
handlers — a fault card on the publish handler sees *neither* of its two
topic publishes today. The field-drafted scenario ("the version-topic publish
succeeded, the fault hit before the template-topic publish — what already
happened?") is precisely the partial-fan-out answer responders need and
currently cannot get; it ships as the `FanOutDual` fixture (§9).

## 7. CX-4 — sensitive-flow vocabulary (no new engine)

The taint lane, scoped to what the acceptance criterion admits:

- A documented rule pack (usage.md section + fixture policy) expressing the
  real bug classes as existing families: *"PII loaders never reach a log
  sink"* (`must_not_reach`, from `pii:*`-selected loaders to logging FQNs),
  *"untrusted entrypoints reach raw SQL only through the sanitizer"*
  (`must_pass_through`).
- The honest semantics stated where the rule is declared: these are
  **call-reachability** claims. A pass proves no call path exists (modulo
  disclosed blind spots) — the strong, testing-can't-give-you direction. A
  violation is a path that *can* carry the data, not a proven flow — triaged
  with the same allow-list discipline as layering.
- Nothing else. No source/sink/sanitizer engine, no dataflow, until the
  distilled-learnings trigger fires in the field. If CX-4's rules prove noisy
  on real services (E-CX4), the answer is rule-shape redesign or removal —
  not a heuristics layer.

**[field] Shipping preconditions, measured rather than guessed.** The rule
pack documents *where this rule may live*, as checkable graph properties, not
advice: a low-blind-spot graph (the clean field service: 0 blind spots, PII
concentrated in ~3 files — a rule there is precise) and a bounded sink count.
On a dense graph with no PII and 84 log sinks behind 107 HighFanOut sites,
the same rule is pure noise and **must not ship** — the pack says so
explicitly, because a noisy rule on one service erodes trust in the family
everywhere. Second measured fact: no scrubber/sanitizer functions exist in
the field at all, so v1 is the waypoint-less `must_not_reach` form;
introducing named scrubbers is an adopter refactor that *upgrades* the rule
to `must_pass_through` later — never a heuristic stand-in.

## 8. CX-5 — cross-service effect chains (observational)

A groundwork fleet surface (`groundwork chains`, and a fleet-MCP lens)
composing facts that already exist per service:

- producer side: the proven ordering around a publish (must-precede verdicts,
  CX-3 effect_order facts) from service A's graph;
- the join: PUBLISH/CONSUME edge labels matched by event name, exactly as
  fleet-events does today;
- consumer side: the consume-handler's proven effects and obligations from
  service B's graph;
- the declared broker assumption from policy
  (`"brokers": {"bus": {"delivery": "at-least-once", "ordered": false}}`) —
  printed on every chain card, never inferred (D-CX5).

The card renders a happens-before chain with each link labeled **proven**
(per-service fact) or **assumed** (broker declaration) — the same legibility
contract as blind spots: exact about structure, explicit about where
structure runs out. Non-gating in v1; a `chain` rule kind that gates ("the
`loan.approved` publish must be commit-dominated in its producer") becomes a
trivial policy check *after* the cards earn field trust — and only if a real
multi-service adopter exists (E-CX5 is an ROI gate, same shape as OB-plan E4).

**[field] The first chain card is already written, and it decomposes onto
this plan's pieces.** The deployment's signed-sentence invariant — *"when a
subscription or event-type version is deleted, its domain row and its outbox
rows are removed in a single committed transaction; never one without the
other"* — splits exactly along the framework's seams:

- *Ordering* — expressible **today**: the two DELETEs share a `RunInTx`
  closure, so existing same-function `effect_order` / `must-precede` cover
  their sequence.
- *Bracketing* ("the outbox DELETE only ever happens inside a transaction") —
  expressible **today** as `must_pass_through` with the tx runner as the
  waypoint: every call chain from an entrypoint to the DELETE passes through
  `RunInTx`, and a chain through `RunInTx` is an in-extent call (the closure
  is invoked *by* the runner). The executor-escapes-the-closure case is the
  disclosed abstention, caught by the existing escape analysis.
- *Pairing* ("both-or-neither") — **blocked on CX-3**: both DELETEs must be
  ALWAYS-effects of the closure (every path through it performs both), which
  together with bracketing and tx semantics yields both-or-neither. The field
  report filed this as "blocked on CX-1"; the precise dependency is the
  summary engine and its ALWAYS-effect application (CX-0 + CX-3) — same
  machinery, earlier phase.

This is why D-CX8 makes CX-5 *dependent* rather than parallel, and the broker
declaration the card prints is the one the field would actually sign:
at-least-once, unordered, idempotent consumers — nothing the code can't back.

## 9. Fixtures

`testdata/groundwork/obligsvc` grows one shape per new verdict path; the
existing shapes keep their verdicts byte-for-byte (the zero-impact half of
the proof):

| function | shape | expected |
|---|---|---|
| `TransferHelper` | acquire; `finish(tx)` commits/rolls back on all its paths | must-release SATISFIED (was VIOLATED-unless-listed) |
| `TransferHelperLeaky` | helper releases on one arm only | CANT-PROVE, detail names the helper |
| `TransferHelperNever` | helper never reaches a release | VIOLATED — unchanged, the D-OB1 worked example |
| `TransferDeferHelper` | `defer closeTx(tx)`, named helper, always releases | SATISFIED (lifts the deferReleases ceiling) |
| `TransferRecursive` | handoff into an SCC | CANT-PROVE (recursion abstention) |
| `TransferDynamic` | handoff through an unresolved interface value | CANT-PROVE (blind frontier) |
| `DisburseWrapped` | `auditAndLog()` (ALWAYS-Require) dominates the publish | must-precede SATISFIED |
| `DisburseWrappedRacy` | the wrapper requires on one arm only | must-precede VIOLATED — unchanged (B undominated by any proven A) |
| `sendFanout` (rule opts in via `fromCallers`, D-CX9) | A in the caller, B in the callee; every entry into the callee dominated — the field's `doPublish`→`publishWithFanout` shape | must-precede SATISFIED via entry domination, witness names the dominated entry |
| `sendFanoutOpen` | same, but its only entry chain never requires | must-precede VIOLATED — unchanged, witness names the A-less entering caller |
| `sendFanoutTaken` | the B-holding helper's address is taken (`var hook = …` in a package initializer) | CANT-PROVE (an unseen dynamic caller may exist) |
| `DisburseRacy` (rule does NOT opt in) | the D-CX9 hazard shape: an incidental upstream require exists in `main` | must-precede VIOLATED — unchanged, because pairing-intent rules never consult callers |
| `ApproveViaHelper` | publish inside an ALWAYS-effect helper, charge call after | CX-3: derived effect_order row with `via` |
| `FanOutDual` | two publishes in one helper, a fallible call between them — the field's partial-fan-out incident shape | CX-3: two derived rows; the fault card answers "first topic published, second not" |
| `OutboxPair` | two DELETEs in a `RunInTx` closure, on every path | ALWAYS-effect facts for both + bracketing `must_pass_through` SATISFIED (§8's decomposition, end-to-end) |

CX-4 adds a `must_not_reach` PII rule to the layeredsvc policy fixture (one
clean route, one violating route, one blind-frontier Caution). CX-5's fixture
is the existing two-service fleet pair with one stitched chain golden.

Unit tables in `internal/static/obligations` cover the summary engine
directly: ALWAYS through nested helpers, NEVER short-circuit, SCC, recover in
a callee, invoke-mode matching in a cone, and the determinism of the
condensation order.

## 10. Build order

Per D-CX8 (the field evidence decided the order, the D-GX1 precedent):

- **CX-0 — summaries.** Engine + unit tables, bottom-up (ALWAYS/NEVER) and
  top-down (ENTRY-DOMINATED). *Exit: every summary row in the table verdicts
  correctly; summary tables byte-stable across checkout paths.*
- **CX-2 — must-precede, both lifts.** Derived A + entry domination; the
  `PublishSplit*` and `DisburseWrapped*` shapes; goldens regenerated. *Exit:
  the field's `validate-before-publish` shape flips VIOLATED → SATISFIED with
  zero rule changes; no new VIOLATED anywhere in the corpus (O-CX2).*
- **CX-3 — derived effect sites.** graphio effect-site collection consults
  ALWAYS-effect summaries; `via` field lockstep. *Exit: `FanOutDual`'s two
  derived rows appear with correct Always; the triage partial-effect answer
  cites them; `OutboxPair`'s pairing facts hold.*
- **CX-1 — must-release credit.** Handoff consultation in `leakPath` +
  deferred-named-helper credit. *Exit: the §9 must-release rows verdict
  correctly end-to-end; monotonicity holds over the whole corpus.*
- **CX-4 — sensitive-flow rule pack.** Pure docs + policy fixtures, zero
  engine code, **zero dependencies — ships any time, scoped by the §7
  preconditions.**
- **CX-5 — chain cards.** After CX-3, and only alongside a real
  multi-service adopter; non-gating; the §8 outbox card is the acceptance
  artifact.

```
CX-0 → CX-2 → CX-3 → CX-1
              CX-3 → CX-5(observational, adopter-gated)
CX-4 (parallel, anytime, clean graphs only)
```

Before the first summary-consuming phase (CX-2) merges: one adversarial
review pass on the summary engine, mirroring the v1 obligations review that
found six semantic bugs — each finding lands as a locked reproduction test,
per the scorecard's bus-factor residual.

## 11. Verifiable outcomes and validation

**Landed correctly — deterministic, machine-checked (CI):**

- **O-CX1 — verdict correctness.** Every §9 shape produces exactly its
  expected verdict, at the unit level and through the golden graph.
- **O-CX2 — trust monotonicity, tested mechanically.** Run the obligations
  check with summaries disabled and enabled over the full fixture corpus;
  assert no finding is VIOLATED in the enabled run that was not VIOLATED in
  the disabled run. This is D-CX2 as a committed test, not a promise.
- **O-CX3 — determinism.** Byte-identical graphs across repeat runs and
  across two checkout paths, summaries included; SCC condensation order
  covered by a dedicated unit test.
- **O-CX4 — zero impact.** Rule-free services byte-identical; rule-bearing
  services with no handoff sites produce identical findings to today.
- **O-CX5 — the gate end-to-end, including the abstention drift.** A branch
  fixture making `finish()` leaky flips the caller SATISFIED → CANT-PROVE: a
  *new Caution* in `review`, and a Violation under `require_proof` — the
  ratchet catches proof-erosion, not just outright leaks.
- **O-CX6 — CX-3 truthfulness.** Derived effect_order rows appear only for
  ALWAYS-effect callees; a some-paths-publish helper produces no derived row
  (negative test).

**Effective — empirical, time-boxed after each phase lands, keep/kill named
now:**

- **E-CX1 — the named field cases.** The first-draft measure here was
  "vocabulary shrink" (count release/require refs that exist only to name
  helpers); the field measured that baseline at **zero** — no obligations
  adopted yet, nothing to shrink — so the keep signal is now concrete cases,
  not a delta: (i) the `validate-before-publish` false-VIOLATED flips to
  SATISFIED via entry domination with zero rule changes; (ii) the
  partial-fan-out fault card cites both publishes through derived sites;
  (iii) the outbox invariant proves end-to-end per §8. *Kill: a lift that
  cannot clear its named case on the field graphs without rule rewrites
  missed the real idiom — rescope that phase before promoting it.*
- **E-CX2 — abstention budget.** Measure new CANT-PROVE findings from UNKNOWN
  handoffs and UNKNOWN entry sets on a real rule set. The field prior is
  encouraging — 0 CANT-PROVE on the trial run, because co-located
  acquire/release keeps interfaces out of the acquire-to-exit window — but
  that measured one idiom, not the class. *Kill threshold: if interprocedural
  abstentions outnumber the false VIOLATED they replaced, the UNKNOWN row is
  too eager — tighten (e.g., consult summaries only for callees that can
  reach a release) or revert to intraprocedural for that rule, never paper
  over with a default.*
- **E-CX3 — soundness audit.** Zero tolerated false SATISFIED: any
  upgraded-by-summary verdict that a human review finds actually leaky is a
  soundness defect (fix-and-lock), never a tuning matter — same posture as
  OB-plan E1.
- **E-CX4 — sensitive-flow noise.** On the first real service configuring a
  PII/sanitizer rule: dismissed-vs-accepted findings, layering's E3
  discipline. *Kill: a rule producing more dismissed than accepted findings
  is removed or reshaped; sustained noise spends trust the whole framework
  runs on.*
- **E-CX5 — the ROI gate.** Chain cards park unless a multi-service adopter
  configures a broker declaration within a quarter of CX-5 landing. Cards
  that exist only on the fixture fleet mean the surface was speculative —
  documented outcome, not a silent shelf. The field's outbox card (§8) is the
  standing first candidate.
- **E-CX6 — the field diff.** The measured deployment has committed to
  running the summaries-disabled vs. -enabled verdict diff on its CI graphs
  against each phase's prototype and returning the deltas. That artifact is
  O-CX2's monotonicity and E-CX1/E-CX2's budgets on a real codebase; **phase
  promotion waits for it** — fixture green alone does not promote.

## 12. Honest limits — and explicit non-goals

Carried limits, stated where users will meet them: VIOLATED remains
existential modulo path feasibility; summaries stop at the analyzed unit's
edge (a release in another module is UNKNOWN, vocabulary is the mechanism
there); must-precede's B side and effect_order's MAY-effects stay
single-function in this plan; chain cards prove code-side links only —
broker behavior is an assumption with a name on it. One forward collision
recorded so it isn't rediscovered: the field's planned auth middleware
(wrapping-closure pattern) will meet `must_pass_through`'s documented
middleware-closure wall when it lands — the waypoint selector needs a design
answer there before that rule can bind.

Non-goals, permanent for this framework rather than deferred: value and logic
correctness (the right amount, the right predicate, the right envelope — the
clamp-constant class from distilled-learnings mode 1), argument-level taint,
and anything requiring a solver or a specification language. Those belong to
tests, property-based tests, and formal methods. The framework's posture
toward that half stays what it is today: abstain legibly
(NO-STRUCTURAL-SIGNAL), and point tests at the gap (`flowmap coverage`) —
the green must keep meaning exactly what it says.
