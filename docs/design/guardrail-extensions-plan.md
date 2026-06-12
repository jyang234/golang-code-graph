# deterministic guardrail extensions: implementation plan

**Status:** implemented — GX-1 through GX-5 all shipped. Kept as the design
record: the extension recipe (§1) is the standing framework policy for every
future check, and D-GX1 records the execution order. Companion to
[`path-obligations-plan.md`](path-obligations-plan.md) and
[`incident-triage-plan.md`](incident-triage-plan.md); this document carries the
extensions that are **pure groundwork** (call-graph level, no SSA) plus the
pre-edit agent feedback loop.

The framework's goal frames every item here, in both directions:

- **agent guardrails** — deterministic, declared invariants so AI-generated
  code lands with consistent, predictable quality, gated before merge;
- **reviewer legibility** — as output volume exceeds what a human can carefully
  read, the artifacts must let the reviewer understand *what is being
  delivered* and *what is still binding* without reading every line.

---

## 1. The extension recipe (framework policy)

Every check in this system — existing or future — follows the same five steps,
and an extension is planned by answering them:

1. **Declare** — a CODEOWNERS-gated rule. *Rule home follows the data:* checks
   needing SSA/CFGs live in `.flowmap.yaml` (flowmap evaluates them); checks
   over the call graph live in `policy.json` (groundwork evaluates them). The
   agent under review never authors its own rules.
2. **Analyze** — a pure function of (source-derived artifact, rules). No
   sampling, no heuristics, no AI in the verdict path.
3. **Disclose** — findings in an existing envelope: graph.json `obligations[]`
   (open `kind` registry, D-OB5) for flowmap-side checks; `fitness.Finding`
   for groundwork-side checks. New graph.json sections need a reason.
4. **Judge** — three-valued always: violation / caution-as-disclosed-abstention
   / no finding. Finding identity is the site, never the prose (D-OB6), so the
   base-vs-branch new-findings diff and the digest work unchanged.
5. **Prove** — a fixture shape per verdict, golden-regenerated.
6. **Validate** — at two levels, declared in the plan before code:
   - *landed correctly* — deterministic outcomes a machine checks: fixture
     verdicts, byte-identical repeat runs, the gate firing end-to-end on a
     seeded branch;
   - *effective* — an empirical signal that the check delivers its stated
     value (catch rate on seeded defects, suppression/abstention rates,
     reviewer or agent outcomes), **with a keep/kill threshold named up
     front**. A check that is correct but never fires on anything real, or
     fires mostly noise, is removed — an inert or noisy guardrail erodes
     trust in every other one.

**Acceptance criterion** for any proposed check: it must be expressible as a
pure function with a sound abstention. Anything heuristic, sampled, or
value-semantic fails the criterion and belongs outside this framework.
(Effectiveness validation is exempt from the determinism requirement — it
measures the framework, it is not part of any verdict path.)

## 2. GX-1 — `must_pass_through` (call-graph waypoint invariant)

The interprocedural sibling of must-precede, needing no SSA: *"every path from
any HTTP entrypoint to `boundary:db` must pass through `authz.Check`"*. This
covers the classic agent failure mode — wiring a new route that skips the
auth / validation / tenancy layer — which layering (package-granular) and
intraprocedural obligations (function-granular) both miss.

**Policy schema** — a sibling of `ReachRule`
(`internal/groundwork/policy/policy.go:68-73`), same selector style, same
`require_proof` escalation, same `Exception` allow-list shape:

```json
"must_pass_through": [
  {"name": "auth-guards-db",
   "from": ["entrypoint:*"],
   "to": ["boundary:db"],
   "through": ["example.com/svc/internal/authz.Check"],
   "require_proof": true,
   "allow": [{"from": "example.com/svc.main", "reason": "composition root"},
             {"from": "...Healthz", "to": "boundary:db SELECT health", "reason": "unauthenticated probe"}]}
]
```

**Selector semantics (v1).** graph.json carries no entrypoint-kind tag, so
`entrypoint:*` is defined as **all graph sources** (nodes with no first-party
callers — `Index.Sources`). This is deliberate: an FQN-glob `from` (e.g. "the
handler package") would let a *new handler package* silently escape the rule —
the exact agent failure mode this check exists to catch. Sources auto-cover
new packages; `main` / health probes are exempted via the allow-list.
Sub-classifying entrypoints by kind (http vs consumer) would require the
boundary contract as a second fitness input — deferred until a rule actually
needs the distinction. Plain FQN globs remain valid `from` selectors for
narrower rules.

**Algorithm.** Remove the `through`-matching nodes from the index; BFS from the
`from`-matching sources; if a `to`-matching target is still reachable, a bypass
path exists → `Violation` naming the (source, target) witness pair, with **one
shortest bypass path in the finding's detail** (deterministic: BFS over the
already-sorted adjacency) — the reviewer sees *how* the guard is skipped, not
just that it is. Per D-OB6 the path is presentation only, never part of the
finding key. Blind spots / `<dynamic>` edges on the traversed frontier →
`Caution` ("cannot prove every path is guarded"), escalated by
`require_proof` — exactly `must_not_reach`'s discipline.

**Build.** New `checkMustPassThrough` in fitness + policy schema + fixture
route in `layeredsvc` (one guarded route SATISFIED, one bypass VIOLATED, one
through-a-blind-spot Caution). Pure groundwork; **no dependency on the
obligations phases — may run in parallel with them.**

## 3. GX-2 — blind-spot ratchet (the meta-guardrail)

As agents generate volume, growth in dynamic dispatch quietly erodes the
soundness of *every other check* — the substrate itself needs a drift ratchet.

**Rule:** no new `blind_spots[]` entries base→branch without an allow-list
entry.

**Build.** `review` diffs blind spots by (kind, site) alongside the existing
findings diff and adds a `new_blind_spots` section to the artifact (additive
artifact schema + digest change). Gate behavior is policy-controlled:

```json
"blind_spot_ratchet": {"gate": true, "allow": [{"site": "...", "reason": "..."}]}
```

Absent from policy → reported in the artifact, non-gating (observe first, the
post-hoc-ingestion discipline). `gate: true` → a new unallowed blind spot is a
`Violation` in `verify`. Tiny diff over already-emitted data; no flowmap
change. Note: the additive artifact field changes the review digest — harmless
because `verify-artifact` recomputes from source rather than trusting stored
digests, but the artifact goldens regenerate in the same commit.

## 4. GX-3 — concurrency-shape invariants

The edge schema already carries `concurrent` (go/defer call sites) — unused by
any check today. Rule kind, again a `ReachRule` sibling:

```json
"no_concurrent_reach": [
  {"name": "no-db-writes-in-goroutines", "to": ["boundary:db INSERT", "boundary:db UPDATE"]}
]
```

**Semantics:** no `to`-matching target may be reachable along a path *entered
via a concurrent edge* (BFS seeded at concurrent-edge targets). Catches the
agent pattern of "make it async" introducing unsupervised writes /
fire-and-forget effects.

**Disclosed limitation:** the flag currently conflates `go` and `defer` sites.
If defer noise appears in practice, the fix is a small flowmap schema
refinement (split the flag) — a lockstep change planned then, not now.
Sequenced after GX-1 (shares the selector machinery).

## 5. GX-4 — suppression audit (`groundwork exceptions`)

Allow-lists accumulate: layering's today; GX-1's, GX-2's, and obligations'
tomorrow. Unaudited, the framework's honesty migrates into an unreviewed
exception graveyard — defeating the legibility goal.

**Build.** A read-only surface, `groundwork exceptions <policy> <graph>
[--json]`, that deterministically lists **every active suppression** across
all rule kinds (rule, from/to/site, reason), and — symmetric to the dead-rule
disclosure in the obligations plan — flags **dead exceptions**: allow-list
entries matching no current edge/site, which are stale and should be deleted.
Exit 0 always in v1 (it informs review, it doesn't gate); a
`max_dead_exceptions` budget can make it gate later if graveyards form.
Sequenced after GX-1 so there is more than one exception source to aggregate.

## 6. GX-5 — pre-edit grounding: `ground` cards + agent feedback loop

Everything else in the framework is *post-hoc judgment*: the agent writes
code, the gate fires. Deterministic **prevention** is strictly cheaper than
deterministic rejection: let the agent ask *before* editing, "what rules bind
the code I'm about to touch?" This revives the original plan's deferred
Phase 5 `ground` surface with a sharper purpose.

**The card** (`internal/groundwork/ground`, CLI `groundwork ground <fqn>`),
pure function of (graph, policy):

```
identity      sig, tier, package's layer position
neighborhood  callers / callees (1 hop), entrypoint cover
effects       reachable boundary effects (db, bus, peers)
binding rules layering constraints on its package; obligations anchored in or
              verdicting on this function; must_pass_through rules where it is
              a waypoint or on a guarded path; budgets on routes through it
blind spots   gaps touching any claim above
```

**The loop:** the card and a `rules(scope)` query ship as MCP tools in the
incident-triage plan's Phase IT-4 server (`ground(fqn)`, `rules(pkg-or-fqn)`,
alongside `triage`/`reach`/`fail`) — one library, one server, both the
incident lens and the pre-edit lens. An agent's edit loop becomes: ground →
edit → verify, with the same deterministic rules at both ends.

**Build.** Basic card (identity / neighborhood / effects / blind spots) needs
only the existing index — buildable any time. The *binding rules* section
reaches full value after obligations Phase OB-2 (obligations in graph.json)
and GX-1 (waypoint rules exist). MCP packaging lands with IT-4.

## 7. Sequencing across all three plans

```
OB-0 → OB-1 → OB-2 → OB-3            (flowmap+groundwork, the SSA track)
GX-1 → GX-3 → GX-4                   (pure groundwork, parallel to OB track)
GX-2                                  (pure groundwork, anytime, smallest)
        OB-2 ─┐
IT-0 → IT-1 → IT-2 → IT-3 → IT-4     (triage track; IT-3 blocked on OB-1/2)
GX-5 basic card: anytime  │  full card: after OB-2 + GX-1  │  MCP: with IT-4
```

The two parallel tracks share no files until `fitness.Check` (one new
`check*` line each — trivial merges).

**D-GX1 — decided start order (supersedes D-OB3's "obligations first"):
GX-2 → GX-1 → OB-0..3 → IT-0..2 → GX-3 → GX-4 → GX-5/IT-3/IT-4.** The ratchet
and the auth-path guard are the highest guardrail-value-per-line in the whole
portfolio, have zero dependencies, and require no schema lockstep — they ship
while the SSA track is still in its first phase.

## 8. Verifiable outcomes and validation

Per the recipe's step 6: *landed* outcomes are deterministic and CI-checked;
*effective* outcomes are empirical with named keep/kill thresholds.

**GX-1 `must_pass_through`:**

- *Landed:* layeredsvc fixtures verdict correctly (guarded → no finding,
  bypass → Violation with deterministic witness path, blind frontier →
  Caution, `require_proof` → Violation). The defining test: **add a brand-new
  handler package with an unguarded route to the fixture — the rule fires
  with no policy change** (the property FQN-glob selectors cannot give).
  Allow-listed pairs never fire.
- *Effective:* seeded-bypass trial — across ~5 agent-style edits that add or
  reroute an entrypoint past the waypoint, `verify` blocks 100% (soundness
  defect otherwise). Track allow-list growth per quarter: *kill threshold —
  if legitimate exemptions grow faster than guarded routes, the rule shape is
  wrong for that codebase and gets redesigned, not suppressed into noise.*

**GX-2 blind-spot ratchet:**

- *Landed:* a branch fixture introducing a new dynamic-dispatch site surfaces
  it in the review artifact's `new_blind_spots`; with `gate: true`, `verify`
  blocks; an allow-list entry suppresses exactly that site; base-equal
  branches report none.
- *Effective:* the value claim is that substrate soundness stops drifting.
  Measure the blind-spot count trend on a real service across one quarter of
  (agent-generated) MRs after enabling the gate. *Keep signal: the count is
  flat or each increase has a reviewed allow-list entry. Kill signal: the
  allow-list becomes a rubber stamp (entries added without reasons) — then
  gating is theater and should revert to observe-only.*

**GX-3 `no_concurrent_reach`:**

- *Landed:* fixture with a goroutine-reached forbidden boundary → Violation;
  the same call on the synchronous path → no finding; blind frontier →
  Caution.
- *Effective:* the disclosed go/defer conflation is the named risk. *Decision
  point: if defer-origin false positives appear in the first real
  deployment, the flowmap flag-split ships before the rule is promoted;* if a
  quarter passes with no configured rule on a real service, GX-3 parks (same
  ROI gate as obligations E4).

**GX-4 `groundwork exceptions`:**

- *Landed:* the listing covers every suppression source that exists at build
  time (layering, GX-1, GX-2), deterministically ordered; deleting a
  fixture's allow-listed edge flags that entry as dead on the next run.
- *Effective:* on first run against a real policy, every listed exception is
  either justified (has a reason) or deleted within the review cycle —
  *the measurable outcome is dead-exception count reaching and holding zero.*
  If the surface is never consulted in review after a quarter, it parks.

**GX-5 ground cards + feedback loop:**

- *Landed:* cards are byte-identical across runs; on fixtures, the *binding
  rules* section names exactly the rules that demonstrably fire on that
  function (cross-checked by seeding a violation at the function and
  asserting the named rule is the one that catches it) — the card never
  promises a guardrail that is not actually binding.
- *Effective:* the clearest experiment in the portfolio, and the one closest
  to the framework's stated goal. A/B an agent on ~10 small tasks against a
  rule-bearing service: with `ground`/`rules` consulted before editing vs.
  without. *Measure: first-attempt `verify` block rate.* The value claim —
  deterministic prevention is cheaper than deterministic rejection — predicts
  a materially lower block rate with grounding. If the rate does not move,
  the pre-edit loop is not earning its MCP surface and IT-4 ships without it.
