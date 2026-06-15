# policy coverage extensions: pressure-tested plan

> **`DESIGN RECORD`** · implemented (PC-4 parked); kept as design record · _reviewed 2026-06-13_

**Status:** PC-1, PC-2, and PC-3 implemented (rule liveness in fitness + the
exceptions audit; `route_io_deltas` and `new_write_targets` in the review
artifact and gate; `effect_ratchet` in the policy schema). PC-4 stays parked
on its named trigger — the first legitimately over-budget route. Kept as the
design record: the pressure test in §2 is why the shipped shapes differ from
the first sketches. Companion to
[`guardrail-extensions-plan.md`](guardrail-extensions-plan.md); follows its
extension recipe (declare / analyze / disclose / judge / prove / validate) and
its acceptance criterion: every check is a pure function of (graph(s), policy)
with a sound abstention. Origin: a coverage review of the six policy families
asking "what can still rot or drift silently?" — every candidate here was
attacked before it was planned, in the discipline of
[`pressure-test.md`](../groundwork/pressure-test.md), and the corrections are
folded into the designs below.

The attacker model is the standing one: the agent under review writes code and
tests, can read the policy, but never authors the policy and never supplies
the graphs (trusted-CI condition). The new angle this plan covers is **rot and
drift of the guardrails themselves** — the meta-axis: rules that silently stop
binding, external surface that grows one ungated effect at a time, and budgets
that go decorative the first time a legitimate exception appears.

---

## 1. The four candidates

| | Extension | Lives in | Catches |
|---|---|---|---|
| PC-1 | rule liveness (inert-rule disclosure) | fitness + exceptions audit | a rule whose `from` no longer binds anything — the guard that quietly stopped existing |
| PC-2 | per-route I/O deltas | review artifact | a route gaining or **losing** reach to external writes, invisible to the global effect diff |
| PC-3 | boundary-write-surface ratchet | review + policy | a new external write target landing without review |
| PC-4 | per-route budget allowances | policy + fitness | the global `io_budget` cap going decorative at the first legitimate exception |

All four are pure groundwork — no flowmap changes, no new graph data, no
schema lockstep.

---

## 2. Pressure test — attacks and corrections

### Attack on PC-1: rename-to-inert

**Attack:** the agent renames `internal/payments` to `internal/billing`. Every
`must_not_reach` / `must_pass_through` rule whose `from` names the old path
now binds nothing: `expandFroms` returns empty, the check loop runs zero
times, and the verdict is `provenAbsent` — a clean pass, forever, including
for `require_proof` rules. Today nothing reports this; the exceptions audit
catches dead *allow entries* but not dead *rules*, and load-time validation
cannot catch it because liveness is graph-dependent.

**Result: attack succeeds against the current system.** PC-1 exists to close
it: a rule whose expanded `from` binds no node is a Caution ("from binds
nothing — inert rule"). For a `require_proof` rule the Caution escalates to a
**Violation**: `require_proof` means "unprovability must not pass CI", and a
rule that cannot even be evaluated is the strongest form of unprovability.

### Attack on PC-1: the born-inert rule

**Attack:** a rule is added to the policy already inert (typo'd FQN in the
same MR). The review diff reports only findings *new on the branch* — but the
inert-rule Caution is present identically on base and branch (both graphs are
judged under the same policy), so the new-findings filter suppresses it
**forever**. The review surface alone cannot see a rule that was never alive.

**Result: attack succeeds against a Caution-only design.** Correction: rule
liveness must also live on an **absolute** surface, not just a delta one. The
`groundwork exceptions` audit gains a policy-liveness section listing every
rule's binding state per pattern, independent of any base/branch pair. The
Caution covers drift (live → inert across a change); the audit covers birth
defects and steady-state rot.

### Attack on PC-1: noise via prophylactic rules

**Attack:** not by the agent — by the policy author. A rule written for code
that does not exist yet cautions on every run; muted checks erode trust in
every other check (the inert-guardrail lesson, recursively).

**Correction, two-part:** (a) the Caution fires only when the **whole** `from`
binds nothing; a partially-dead pattern list (one live glob, one dead) is
audit-level disclosure (per-pattern LIVE/DEAD), not a Caution. (b) A dead `to`
in `must_not_reach` is **deliberately not inert** — "the forbidden thing does
not exist" is the success state, and flagging it would train reviewers to
ignore the disclosure. A dead `to` is listed in the audit as INFO only, with
the honest framing that it may be success (target removed) or rot (target
renamed and the rule now vacuous) — the reviewer judges. A dead `through` in
`must_pass_through` needs no special case: removing zero waypoints makes every
`from`→`to` path a bypass, so that rot fails loud already.

### Attack on PC-2: the percentage gate

**Attack:** gate on "write count grew ≥200%". Two breaks. First, the count is
*distinct static write targets reachable from the route*, not runtime volume —
the N+1 caveat means "3 targets instead of 1" gets conflated with "3× the
I/O", and the gate asserts something the graph does not know. Second, the
agent defeats any per-route ratio by splitting the fat route into two new
routes, each individually unremarkable.

**Result: the percentage gate is killed, not corrected.** What survives is
**disclosure of any per-route write-set delta** (deterministic, ungameable —
there is no threshold to duck under) plus the existing absolute cap
(`io_budget`), which is already the declared-policy way to say "too many".
No percentage semantics anywhere.

### Attack on PC-2: rename laundering

**Attack:** rename the handler so the route appears as removed + added and
the count comparison has no common key — a count increase hides in a "new"
route.

**Result: attack fails against the existing system.** The per-route delta lists
the renamed handler's before/after as two **one-sided rows** (keyed on handler
FQN), so the count never gains a common key to hide in — the increase shows on
the new row regardless. Note that renaming a handler while the *route name* is
unchanged is correctly NOT a breaking contract change (`review.contractChanges`
keys the entrypoint contract on the route name, not the handler FQN — R10): the
external surface is the same route, so the gate stays quiet on the contract while
the one-sided rows still expose the count. The laundering defense is the
per-route attribution, not the contract gate.

### Attack on PC-2: blind-frontier count shifts

**Attack:** none needed — the substrate shifts on its own. A count drop caused
by the branch graph going blind (or a rise caused by base blindness resolving)
would render as a behavior change when it is an epistemic one.

**Correction:** each side of a delta row carries the frontier state
(`resolved` / `blind`, reusing the reach checks' frontier probe). A delta with
a blind side renders with the marker; the three-valued discipline applies to
counts exactly as it does to reach verdicts.

### Attack on PC-3: dynamic laundering

**Attack:** the agent adds the new external write behind dynamic dispatch, so
the effect label is `<dynamic>` and — if a dynamic write effect already exists
in base — the set-based effect diff shows no new label. The ratchet sees
nothing.

**Result: attack is caught, but by a different check** — introducing the
dispatch site creates a new blind spot, and the blind-spot ratchet (GX-2)
fires. **This is a documented dependency, not a coincidence:** PC-3's
soundness against laundering leans on GX-2 being gated. A policy that gates
the effect ratchet but not the blind-spot ratchet is disclosed as weaker
(`groundwork exceptions` can state it). Residual, shared with pressure-test
attack 3: a new *use* of an already-labeled effect through an already-blind
path is invisible to both ratchets — that is PC-2's job at route granularity,
and the three surfaces triangulate.

### Attack on PC-4: prefix inheritance

**Attack:** a budget allowance matched by prefix ("the batch package may make
12 writes") is inherited by every **new** route the agent adds to that package
— the exact failure mode `entrypoint:*` was designed to prevent in
`must_pass_through`.

**Correction: budget allowances match the route FQN exactly** — a deliberate,
documented deviation from the house exact-or-prefix convention, because the
matched object is an exemption that new code must never inherit silently. A
renamed or deleted route leaves the entry dead, and the exceptions audit
flags it (exact matching makes deadness unambiguous).

---

## 3. PC-1 — rule liveness

**Declare.** Nothing new — liveness is a property of every existing
From-bearing rule. No schema change.

**Analyze.** After `expandFroms`, an empty seed set is the inert condition.
Per-pattern liveness: a pattern is live iff it matches ≥1 node (or is
`entrypoint:*` with ≥1 source).

**Disclose / judge.**
- Whole-`from` inert → `Caution` on the rule's own kind ("from binds nothing —
  inert rule"); `require_proof` escalates to `Violation`. Finding identity is
  (rule kind, rule name, inert marker) — site-style, prose-free (D-OB6), so
  the base-vs-branch diff and digest work unchanged.
- Per-pattern liveness for `from` / `through` (LIVE/DEAD) and `to`-liveness as
  INFO → a policy-liveness section in the `groundwork exceptions` output,
  absolute (no base graph needed), deterministically ordered.

**Prove.** Fixture trio: rename the guarded package on a branch → new Caution
in the artifact; same with `require_proof` → Violation, BLOCK; a born-inert
rule → absent from the review diff (asserting the suppression is real) but
present in the audit listing.

**Validate.** *Landed:* the fixtures above, byte-identical repeat runs.
*Effective:* on the dogfood policy, inert-rule count at steady state is zero
and any Caution is resolved (rule fixed or deleted) within one review cycle.
*Kill signal:* if inert-rule Cautions start being suppressed rather than
fixed, the disclosure has become noise and the design (not the threshold)
gets revisited.

## 4. PC-2 — per-route I/O deltas

**Declare.** Nothing required for v1 — the section is disclosure, computed
unconditionally when `review` runs (like `io_effects`). Any future gate (e.g.
on lost writes) must be a declared policy field; none ships now.

**Analyze.** Factor the route → distinct-write-target map out of
`checkIOBudget` into a shared helper (same Sources / root-exemption / IsWrite
semantics — one matcher, per the MatchesAny lesson). Compute it for base and
branch; emit a row for every route whose write set differs, plus one-sided
rows for appearing/disappearing routes. Each side carries its frontier state.

**Disclose.** New additive artifact section:

```json
"route_io_deltas": [
  {"route": "...Server.UpdateUser",
   "base": {"writes": 2, "frontier": "resolved"},
   "branch": {"writes": 3, "frontier": "resolved"},
   "added": ["db INSERT audit_log"], "removed": []}
]
```

Additive schema changes the digest; artifact goldens regenerate in the same
commit (the GX-2 precedent). Render groups by effect when one change fans out
across many routes — "this put `db INSERT audit_log` behind 40 routes" is the
signal, not 40 rows of noise; the JSON stays complete.

**Judge.** No verdict change. A route-delta row implies a non-empty graph
delta, so the artifact is already at least STRUCTURALLY-CLEAR; rows never
Block in v1. The high-stakes direction is the **lost write** (a route that
reached `db INSERT audit_log` in base and no longer does, while the global
effect set is unchanged because another route still writes it — invisible to
every existing surface). That row partially closes failure mode 5 of the
design record: absence of a thing that was never there is unverifiable, but
the *disappearance* of a thing that was there is deterministic.

**Prove.** Fixture: sever one route's path to a shared write while a second
route keeps it → exactly one row with the removed write **and** an empty
`io_effects` diff (the test asserts the gap this section exists to close);
a blind-side fixture renders the frontier marker; a route rename renders the
one-sided rows alongside the existing breaking contract change.

**Validate.** *Landed:* fixtures above; byte-identical repeats. *Effective:*
the section is consulted — at least one review outcome per quarter is changed
or confirmed by it (the GX-4 standard). *Kill signal:* a quarter of real MRs
where the section never informs a review → park it; *gate trigger:* if a lost
write slips through review despite the row, a declared `gate_on_lost_writes`
policy field (with an allow-list for intentional removals) is the planned
escalation — not before.

## 5. PC-3 — boundary-write-surface ratchet

**Declare.** Sibling of `blind_spot_ratchet`, same lifecycle:

```json
"effect_ratchet": {
  "gate": false,
  "allow": [{"target": "db INSERT audit_log", "reason": "Q3 audit feature"}]
}
```

`target` matches the effect label exact-or-prefix; empty target rejected at
load (the empty-pattern rule). Absent from policy → reported, never gating
(observe first).

**Analyze.** Diff the *write* effect-label sets base → branch (labels via the
existing `IsWrite` / `TrimPrefix` classification); new labels not covered by
`allow` are the findings. Reads are deliberately out of scope v1 — outbound
read additions already surface as contract changes, and a read ratchet ships
only if surface drift is observed there (named limitation).

**Disclose / judge.** New unallowed write targets → a `new_write_targets`
artifact section; `gate: true` makes each a Violation in verify/review (the
GX-2 shape exactly). A new `<dynamic>` write label is a finding like any
other; laundering through an *existing* dynamic label is the documented GX-2
dependency from the pressure test. Allow entries feed the exceptions audit
(LIVE/DEAD by whether the label exists in the current graph).

**Prove.** Fixtures: branch adds a write to a new table → finding; `gate:
true` → BLOCK; allow entry suppresses exactly that label; dynamic-laundering
fixture asserts the blind-spot ratchet fires where the effect ratchet cannot
(the dependency is a test, not a comment).

**Validate.** *Landed:* fixtures above. *Effective:* external write-surface
growth on a real service is flat or every increase carries a reviewed allow
entry with a reason. *Kill signal:* the allow-list becomes a rubber stamp —
then gating is theater and the ratchet reverts to observe-only (GX-2's own
standard).

## 6. PC-4 — per-route budget allowances

**Declare.**

```json
"io_budget": {
  "max_writes_per_route": 6,
  "allow": [{"route": "(*example.com/svc/internal/batch.Job).Run",
             "max": 12, "reason": "nightly reconciliation fans out by design"}]
}
```

`route` matches **exactly** (the pressure-test correction — no prefix, no
inheritance by new routes); `max` ≥ 0; both required; an allowance is still a
cap, never an exemption.

**Analyze / disclose / judge.** `checkIOBudget` consults the allowance for
the route's cap; over-allowance is the same `io_budget` Violation naming the
route's own cap. Entries feed the exceptions audit (DEAD when no current
source matches — a renamed route goes dead loudly, and the rename itself is
already a breaking contract change).

**Prove.** Fixtures: over-budget route with an exact allowance passes; a new
route in the same package does **not** inherit and violates; the stale entry
after a rename lists DEAD.

**Validate.** *Landed:* fixtures above. *Build trigger, per the keep/kill
discipline:* this ships **on first need** — the first legitimately
over-budget route on a real policy (the dogfood currently passes at 6) — not
speculatively. *Kill signal:* allowances outnumbering un-allowed routes means
the global cap is wrong, and the budget gets re-derived from the graph
(lesson 2), not allow-listed into noise.

---

## 7. Sequencing and file contention

```
PC-1 (fitness + exceptions audit)   — first: highest value-per-line, closes a
                                      named failure mode, no schema change
PC-2 (review artifact)              — second: pure disclosure, rides PC-1's
                                      frontier-probe reuse
PC-3 (policy schema + review)       — third: needs the observe-first soak that
                                      PC-2's section informs
PC-4 (policy schema + fitness)      — on first need, smallest
```

Shared surface: all four touch the exceptions audit (one section each —
trivial merges, the GX pattern); PC-2/PC-3 both extend the artifact schema
(digest changes regenerate goldens once per landing). Nothing here blocks on,
or is blocked by, any OB/IT/GX item.

## 8. What was considered and not folded in

- **Percentage-change gates** — killed in the pressure test (static counts ≠
  volume; split-route gaming); replaced by full disclosure + the existing
  absolute cap.
- **Allow-lists on `must_not_reach` / `no_concurrent_reach`** — deferred until
  a real policy needs one; a negative invariant arguably should not have
  holes, and narrowing `from` remains the honest escape.
- **Dead-code ratchet** ("no new unreachable first-party functions") —
  hygiene, not safety; parked until an agent-volume problem is observed, and
  only with a named kill threshold.
- **`max_dead_exceptions` as a gate** — already named in GX-4; a five-line
  change whenever a graveyard actually forms.
- **CFG / value-flow / entrypoint-kind subclassing** — standing refusals,
  unchanged.
