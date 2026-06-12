# correctness expansion: implementation plan

**Status:** proposed — CX-0 through CX-5 not started. Companion to
[`path-obligations-plan.md`](path-obligations-plan.md) (whose §4/§10
"no interprocedural" limit D-CX2 supersedes, for *proven* summaries only) and
[`guardrail-extensions-plan.md`](guardrail-extensions-plan.md) (whose §1
extension recipe governs every check here). Grounded in the actual code (file
references verified), not in aspiration.

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

## 5. CX-2 — interprocedural must-precede (the Require side only)

A plain call to a callee whose summary is ALWAYS-calls-Require counts as a
**derived A site**: if it dominates a B site, every path genuinely executed
the require before the B — sound, and it flips exactly the false-VIOLATED
class (audit write wrapped in a named helper).

The **B side stays intraprocedural** in this slice, disclosed in the kind's
doc comment: deriving B sites from callees that *may* reach a B would mint
new VIOLATED findings from over-approximated cones — exactly what D-CX2
forbids. A publish hidden inside a helper therefore still escapes this rule
in v1; the honest statement of that limit ships with the feature, and lifting
it (ALWAYS-calls-B derivation, which is sound but partial) is a named
follow-on, not a silent gap.

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

This phase is what makes CX-5 possible: cross-service chains compose
*proven* per-service ordering facts, and same-function-only facts are too
sparse to compose.

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
| `ApproveViaHelper` | publish inside an ALWAYS-effect helper, charge call after | CX-3: derived effect_order row with `via` |

CX-4 adds a `must_not_reach` PII rule to the layeredsvc policy fixture (one
clean route, one violating route, one blind-frontier Caution). CX-5's fixture
is the existing two-service fleet pair with one stitched chain golden.

Unit tables in `internal/static/obligations` cover the summary engine
directly: ALWAYS through nested helpers, NEVER short-circuit, SCC, recover in
a callee, invoke-mode matching in a cone, and the determinism of the
condensation order.

## 10. Build order

- **CX-0 — summaries.** Engine + unit tables. *Exit: every summary row in the
  table verdicts correctly; summary tables byte-stable across checkout
  paths.*
- **CX-1 — must-release credit.** Handoff consultation in `leakPath` +
  deferred-named-helper credit; obligsvc shapes; goldens regenerated. *Exit:
  the §9 must-release rows verdict correctly end-to-end; the monotonicity
  check (O-CX2) passes over the whole fixture corpus.*
- **CX-2 — must-precede derived A.** *Exit: wrapped-audit shapes verdict
  correctly; no new VIOLATED anywhere in the corpus.*
- **CX-3 — derived effect sites.** graphio effect-site collection consults
  ALWAYS-effect summaries; `via` field lockstep. *Exit: the derived
  effect_order row appears with correct Always; triage partial-effect answers
  cite it.*
- **CX-4 — sensitive-flow rule pack.** Pure docs + policy fixtures, zero
  engine code, **zero dependencies — may ship any time, in parallel.**
- **CX-5 — chain cards.** After CX-3, and only alongside a real
  multi-service adopter conversation; non-gating.

```
CX-0 → CX-1 → CX-2 → CX-3 → CX-5(observational, adopter-gated)
CX-4 (parallel, anytime)
```

Before CX-1 merges: one adversarial review pass on the summary engine,
mirroring the v1 obligations review that found six semantic bugs — each
finding lands as a locked reproduction test, per the scorecard's bus-factor
residual.

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

- **E-CX1 — vocabulary shrink.** Re-express loansvc's rules; count the
  release/require refs that existed only to name helpers. *Keep signal: the
  count drops materially (the false-flag class is really gone). Kill: if
  proven-ALWAYS helpers are rare in real code, the credit is hollow — the
  vocabulary mechanism stays primary and CX-2/3 are re-scoped.*
- **E-CX2 — abstention budget.** Measure new CANT-PROVE findings from UNKNOWN
  handoffs on a real rule set. *Kill threshold: if interprocedural abstentions
  outnumber the false VIOLATED they replaced, the UNKNOWN row is too eager —
  tighten (e.g., consult summaries only for callees that can reach a release)
  or revert to intraprocedural for that rule, never paper over with a
  default.*
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
  documented outcome, not a silent shelf.

## 12. Honest limits — and explicit non-goals

Carried limits, stated where users will meet them: VIOLATED remains
existential modulo path feasibility; summaries stop at the analyzed unit's
edge (a release in another module is UNKNOWN, vocabulary is the mechanism
there); must-precede's B side and effect_order's MAY-effects stay
single-function in this plan; chain cards prove code-side links only —
broker behavior is an assumption with a name on it.

Non-goals, permanent for this framework rather than deferred: value and logic
correctness (the right amount, the right predicate, the right envelope — the
clamp-constant class from distilled-learnings mode 1), argument-level taint,
and anything requiring a solver or a specification language. Those belong to
tests, property-based tests, and formal methods. The framework's posture
toward that half stays what it is today: abstain legibly
(NO-STRUCTURAL-SIGNAL), and point tests at the gap (`flowmap coverage`) —
the green must keep meaning exactly what it says.
