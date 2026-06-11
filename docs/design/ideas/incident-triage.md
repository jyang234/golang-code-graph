# Idea: incident triage — blast radius and failure-scenario exploration

**Status:** exploratory idea, not built. This distills a design discussion; it is
a proposal to weigh, not a committed plan.

Related reading: the `impact` surface and failure-mode analysis in
[`../../groundwork/distilled-learnings.md`](../../groundwork/distilled-learnings.md);
groundwork's built `reach` surface in
[`../../groundwork/usage.md`](../../groundwork/usage.md); flowmap's post-hoc trace
path in [`../post-hoc-behavioral-ingestion.md`](../post-hoc-behavioral-ingestion.md);
the ordering/lifecycle idea in [`path-obligations.md`](path-obligations.md).

---

## The goal

In an incident, arm a responder (a human or Claude) with the easiest way to:

1. **trace the blast radius** of a specific failure — what could have caused this,
   what else is affected, which routes are implicated; and
2. **explore failure scenarios** ("what if dependency P is down?") in a throwaway,
   exploratory way — explicitly **without** authoring a mess of weirdly specific
   test cases.

## The crux: interrogation of a model, not authored artifacts

The "no test cases" constraint is a design signal, not just a preference. A test
is an artifact you write, commit, and maintain forever, asserting one expected
behaviour. Incident exploration is the opposite modality: **throwaway
interrogation** — "what could be going on / what if X fails" — asked now and
discarded. A graph is a *queryable model*, so it answers unlimited what-if
questions without materialising any as committed tests. That is exactly why this
belongs on the graph (and on real telemetry), not in more tests: the moment a
scenario "really runs", it needs a harness plus a scenario spec — i.e. a test. You
cannot have exploratory **and** executed **and** test-free; pick two.

The ask decomposes into three sub-capabilities with very different feasibility.

## (1) Blast-radius tracing — mostly already there, needs an incident front door

groundwork's graph already does this natively: bidirectional reachability,
entrypoint cover, reachable boundary effects, blind spots on the path. The gap is
not capability — it is the **input**. An incident hands you a *symptom*, not an
FQN. The augmentation is a symptom→node resolver plus an incident-shaped output:

| Incident input | Graph query → what it tells the responder |
|---|---|
| panic / stack trace | top frames are FQNs → reverse + forward reach around the failing frame |
| route 500s (`POST /x`) | entrypoint → forward reach = the *bounded suspect set* (all that route can touch) |
| corrupted table | `boundary:db … <table>` edges → which functions write it → their callers → which routes |
| event T missing | the publish edge (who should emit) + consumers of T (who is now starved) |
| dependency P slow/down | `boundary:P …` edges → who calls P → blast radius of P being unavailable |

This is the **`impact`** surface from the design record (transitive
callers/callees, entrypoint cover, blind spots in path) — *designed but not yet
built*. The incident use case is arguably a stronger motivation for it than the
original "agent reads a grounding card before editing." Deterministic, reads the
existing graph, no tests.

## (2) Failure-scenario exploration — split it; the constraint decides which half

- **Static what-if: fault propagation / FMEA over the graph — the strong fit.**
  "If P is down / X fails / T stops publishing, what is the blast radius?" is a
  *graph query*, not an execution. The raw material is already present: the
  `fallible` flag, error paths, reachability, entrypoint cover, boundary effects.
  Mark a node or edge as failing and propagate to entrypoints. Exploratory,
  repeatable, **zero test cases** — you are asking the model, not running anything.

  The high-value move, which ties into [`path-obligations.md`](path-obligations.md):
  **partial-effect / inconsistent-state detection under a fault.** "credit-bureau
  times out → these 2 routes 500, *and on the disburse path a `loan.approved`
  publish fires before the failing charge* → you may have approved-but-uncharged
  loans." That is an ordering-under-failure question — does a committed external
  effect precede the fault on some path — and it is exactly what an incident
  responder is desperate for. Deterministic and graph-derived.

- **Runtime chaos (actually inject the fault and observe) — wrong fit, and the
  constraint already rejects it.** This is chaos engineering; static tools cannot
  do it, and flowmap's behavioural pipeline could only do it by *writing a flow* —
  the "weirdly specific test case" we are trying to avoid. So the no-test
  constraint is not incidental: executed scenarios require an authored spec.

## (3) "What actually happened" without writing a test

One existing capability directly satisfies "no test cases" for a *specific*
incident: flowmap's **post-hoc trace ingestion** (`flowmap behavior ingest`, the
OTLP path). In an incident you usually already have the production OTel trace of
the failing request. Feed it in, canonicalise it, and **diff it against the
golden** for that flow → "here is exactly where this request diverged from
known-good." Behavioural triage driven by the incident's *own telemetry*, not an
authored test.

This is the complement to the static side:

- **static graph (groundwork):** what *can* happen → bounds the search, propagates faults.
- **post-hoc trace (flowmap):** what *did* happen on this incident → locates the actual divergence.

Graph to narrow, telemetry to locate.

## The honest limits

- **Static blast radius over-approximates.** It hands you the *map* (the functions
  a route could touch, the few that write the bad table), not the *actual route
  taken*. It scopes the hunt; the trace/logs pinpoint.
- **It traces the *deployed* code, so you need the graph for that commit.** A real
  reason to produce a graph artifact per deploy — the same trusted-generation
  discipline as the deferred Phase 4. A stale map mis-triages.
- **Structural, not semantic.** Fault propagation enumerates *possible* failure
  blast radii; it does not diagnose root cause. "This path can return an error and
  fire a publish first" — not "this is the bug."
- **Not a third tool.** With one exception (live chaos, which the constraint
  rejects), this is a *new lens over the existing graph + telemetry*, not a new
  system.

## Arming Claude specifically

The packaging for an agent is the MCP surface the design record already lists as
one of groundwork's three faces: a tool like `triage(symptom)` → a structured card
(suspect set, bidirectional blast radius, implicated routes, side-effects-before-
the-fault, blind spots on the path), which the agent then navigates interactively
("now show me who publishes T", "what is reachable from this frame"). The graph is
the substrate; the agent does the exploratory walk; nothing is committed.

## Bottom line

Achievable largely by *recombining what exists*, not by building a new tool:

- **Blast-radius triage** = the designed-but-unbuilt `impact` surface + a
  symptom→node front door (small, deterministic, no tests).
- **Failure-scenario exploration** = static **fault propagation** over the graph,
  including the inconsistent-state-under-fault check (new lens; reuses
  `fallible` / reach / effects + the ordering idea; exploratory, no tests).
- **"What actually happened"** = flowmap's **post-hoc trace ingestion**, fed the
  incident's real trace (already exists; no tests).

The one thing it cannot be is live fault injection — a separate runtime concern
that breaks the no-test constraint by definition. Everything else fits, precisely
because incident triage is *interrogation of a model*, which is what a graph is
for.

## Open questions

- **Symptom→node resolver scope.** Stack frames and routes map cleanly; a raw log
  line or error string is fuzzier. Start with the clean inputs (frame, route,
  table, event, peer)?
- **Fault-propagation semantics.** How far to model error propagation — just
  "reaches an entrypoint as an error", or distinguish *returned error* vs
  *degraded/partial*? The partial-effect case needs intraprocedural ordering (the
  `path-obligations` machinery), so the two ideas share infrastructure.
- **Graph-per-deploy.** Incident triage wants the graph for the *running* commit;
  this presses on the trusted-generation / artifact-retention question (Phase 4).
- **Surface.** A CLI `impact`/`triage` first, or go straight to the MCP tool for
  agent-driven walks? CLI is cheaper to validate; MCP is where the "arm Claude"
  value lands.
