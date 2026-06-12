# incident triage: implementation plan

**Status:** implemented — IT-0 through IT-4 all shipped. Kept as the design
record (D-IT1..5 and the graph-per-deploy prerequisite). Refined from
[`ideas/incident-triage.md`](ideas/incident-triage.md). Grounded in the actual
code (file references verified).

Scope decisions made when this plan was cut:

- **D-IT1 — sequenced after path obligations.** The highest-value capability
  here (partial-effect-under-fault, Phase IT-3) is an intraprocedural ordering
  question and reuses the dominance machinery
  [`path-obligations-plan.md`](path-obligations-plan.md) builds (Phase OB-1).
  Everything before IT-3 has no such dependency.
- **D-IT2 — CLI first, MCP after.** The triage engine is a library from day one
  (`internal/groundwork/impact`), surfaced as `groundwork triage` to validate
  the card format and resolver cheaply; the MCP server (Phase IT-4) wraps the
  same library.
- **D-IT3 — fault propagation v1 is blast-radius only.** Failing node/boundary →
  implicated entrypoints, with blind spots disclosed. No returned-error vs
  degraded distinction in v1; partial-effect detection ships in IT-3.
- **D-IT4 — symptom resolver starts with the clean inputs**: stack frame, route,
  table, event, peer. Raw log lines / error strings are deferred. Ambiguity is
  surfaced as a candidate list, never guessed — same honesty discipline as
  everything else.

The third leg of the idea — "what actually happened" via post-hoc trace
ingestion — **already exists** (`flowmap behavior ingest`, built through
Stage 2; `cmd/flowmap/main.go:199-249`). This plan adds nothing there beyond a
documentation pointer in the triage output.

---

## 1. What exists vs. what this builds (verified)

The substrate is groundwork's graph index
(`internal/groundwork/graph/index.go`): forward/reverse BFS
(`Reachable`/`Reaching`, lines 107-141), boundary-edge map keyed by caller,
entrypoint sources, blind-spot map. `reach` already exposes raw bidirectional
reachability. What is missing is exactly what the idea doc says: a
**symptom-shaped front door** and an **incident-shaped output** — plus fault
framing and, later, ordering-under-fault.

Nothing in IT-0..IT-2 touches flowmap; it is composition over the existing
index, in groundwork only. IT-3 is the one phase that adds a flowmap emission.

## 2. The triage card

One structured output for every entry path (text and `--json`), assembled by
`internal/groundwork/impact`:

```
symptom        what was asked, and how it resolved (or the candidate list)
suspects       resolved node set / boundary edges
blast radius   reverse reach → implicated entrypoints (routes, consumers, mains)
forward reach  what the suspects can touch → boundary effects (db, bus, peers)
blind spots    every disclosed gap on any path used above — the card says
               where its own claims stop being sound
next steps     pointer to `flowmap behavior ingest` when an OTel trace exists
```

Determinism: pure function of (graph, query); sorted output; no scores or
ranking in v1 — ranking is judgment, the card is evidence.

## 3. Symptom → node resolver: `internal/groundwork/symptom`

| input | flag | resolution against the graph |
|---|---|---|
| stack frame | `--frame` | Go frame syntax (`pkg.(*T).Method`) normalized and suffix-matched against node FQNs |
| route | `--route "POST /loans"` | matched against entrypoint sources (HTTP roots carry method+route from root discovery) |
| table | `--table loans` | `boundary:db <op> <table>` edge labels → writing/reading functions |
| event | `--event loan.approved` | `boundary:bus PUBLISH/CONSUME <event>` edges → publishers and consumers |
| peer | `--peer credit-bureau` | `boundary:<peer>` outbound edges → callers |

Multiple matches return all candidates with their distinguishing context
(package path, signature) and a nonzero "ambiguous" marker in JSON; zero
matches name the nearest misses. `<dynamic>` boundary targets are listed as
*possible* matches, flagged as such — a dynamic publish might be the missing
event T.

## 4. Fault propagation (v1: blast radius)

`groundwork triage --fail <resolved symptom>` reframes the same machinery as a
what-if:

- **failing function/frame** → `Reaching` → entrypoints whose paths cross it =
  the implicated routes; plus its own forward boundary effects (what else
  degrades).
- **failing peer** (`--fail --peer P`) → callers of every `boundary:P` edge →
  their entrypoint cover = blast radius of P being down.
- **failing event** (`--fail --event T`) → the publish edge's entrypoint cover
  *and* the consumers of T (now starved) and everything reachable from them.
- **failing table** → analogous via `boundary:db` edges.

This is exploration, not a gate: read-only, exit 0, no policy. Blind spots on
any traversed path are always in the card — a blast radius through a
`<dynamic>` edge or `UnresolvedDispatch` is labeled unsound, not omitted.

The honest limits from the idea doc are restated in the CLI help: this is the
*map* (over-approximate, structural), it scopes the hunt; the trace locates.

## 5. Partial-effect under fault (IT-3, depends on obligations machinery)

The question: *given a fault at call site C, which committed external effects
(bus publish, db mutate) can have already happened on some path?* — "you may
have approved-but-uncharged loans."

**D-IT5 — mechanism: flowmap precomputes per-function effect-order facts; triage
consumes them.** The alternative — groundwork running SSA on demand at incident
time — is rejected: it breaks the producer/judge trust split, and the deployed
commit's *source* may not be at hand while its graph artifact is.

flowmap (which has the CFGs and, after Phase OB-1, the dominance/reachability
walks) emits a second narrow level-2 slice, only for functions that contain
**both** ≥1 committed-effect boundary site (publish / db mutate) **and** ≥1
fallible or boundary call site — a small minority of functions:

```json
"effect_order": [
  {"fn": "example.com/svc/internal/app.Disburse",
   "effect": "boundary:bus PUBLISH loan.approved", "effect_site": "app.go:91",
   "before": ["example.com/svc/internal/billing#Charge"],   // effect can precede these sites
   "always_before": false}                                   // true = effect dominates the site
}
```

Per (effect site E, fallible site C) pair the relation is three-valued:
*can-precede* (a path E→C exists), *always-precedes* (E dominates C), or
neither — derived from the same intra-CFG reachability/dominance primitives as
obligations, with the same `CANT-PROVE` abstentions disclosed. Triage then
answers: fault at C → effects with C in `before` are *possibly committed*;
`always_before` ones are *certainly committed* (on any path reaching C).

Like `obligations`, this is a lockstep schema change (graphio emit + groundwork
strict decode + golden regen in one commit), omitted entirely when empty.

Two implementation notes from scoping (record so IT-3 starts warm): the
computation belongs in **graphio's Build loop**, not the obligations package —
that is the one place where boundary labels, their `ssa` call sites, and the
callee signatures coexist per call-graph edge; and the fixture needs
`classify.busPublish`/`db` hints added to obligsvc's `.flowmap.yaml` (it has
none today, so its publishes are currently unlabeled) plus a
`DisburseAndCharge` shape: audit → publish → fallible charge, where failing
the charge must report the publish as possibly-committed.

## 6. Build order

- **Phase IT-0 — impact engine.** `internal/groundwork/impact`: card assembly
  over the existing index (no new graph queries, only composition).
  *Exit: given an FQN on the loansvc golden graph, the card shows correct
  entrypoint cover, effects, and blind spots; deterministic across runs.*
- **Phase IT-1 — symptom resolver + CLI.** `internal/groundwork/symptom` +
  `groundwork triage` with the five flags, `--json`, ambiguity handling.
  *Exit: each symptom kind resolves on the fixtures, including an ambiguous
  case returning candidates and a `<dynamic>` case flagged as possible.*
- **Phase IT-2 — fault propagation.** `--fail` framing for all five symptom
  kinds, starved-consumer expansion for events.
  *Exit: "peer P down" on loansvc names exactly the routes whose paths cross a
  `boundary:P` edge, with blind spots disclosed.*
- **Phase IT-3 — partial-effect under fault.** flowmap emits `effect_order`
  (reusing Phase OB-1 walks); triage adds the *possibly/certainly committed
  effects before the fault* section.
  *Exit: the disburse scenario reproduces — fail the charge call, the card
  reports `loan.approved` as possibly-committed-before-fault.*
  Blocked on path-obligations Phases OB-1/OB-2.
- **Phase IT-4 — MCP surface.** Wrap the impact library as the agent-facing
  tools (`triage(symptom)`, `reach(fqn)`, `fail(symptom)`), enabling the
  interactive walk ("now show who publishes T"). The same server also carries
  the **pre-edit grounding tools** (`ground(fqn)`, `rules(scope)`) from
  [`guardrail-extensions-plan.md`](guardrail-extensions-plan.md) GX-5 — one
  library, one server, the incident lens and the pre-edit guardrail lens
  together. Design sketch only in this plan; it is a separate effort and the
  first MCP surface in the repo, so it carries its own (small) infrastructure
  decision.

## 7. Verifiable outcomes and validation

**Landed correctly — deterministic, machine-checked (CI):**

- **O1 — resolver correctness.** Each of the five symptom kinds resolves to
  the hand-verified expected node set on the fixtures; an ambiguous symptom
  returns the full candidate list (never a guess); a `<dynamic>` near-match is
  returned flagged as *possible*.
- **O2 — card determinism.** Byte-identical `--json` cards across repeat runs
  for every symptom kind and for `--fail`.
- **O3 — blast-radius exactness.** On the loansvc golden graph, `--fail
  --peer P` names exactly the entrypoints whose paths cross a `boundary:P`
  edge — asserted against a committed, hand-derived expected set, not against
  the implementation's own output.
- **O4 — blind-spot honesty.** A card whose traversal crosses any blind spot
  or `<dynamic>` edge always carries the disclosure; a test constructs the
  crossing and asserts the card is never silently clean.
- **O5 — partial-effect (IT-3).** The disburse scenario reproduces: fail the
  charge site, the card reports `loan.approved` as
  possibly-committed-before-fault; move the publish below the charge, it
  disappears.

**Effective — empirical, time-boxed after IT-2 (E1–E3) and IT-4 (E4):**

- **E1 — staged incident drills.** Seed ~5 fault scenarios into loansvc (peer
  timeout, missing event, bad table write), each with a captured OTel trace.
  For each, measure: (a) *recall* — the true culprit is in the suspect set
  (over-approximation should make this ~always true; a miss indicates a
  resolver gap or stale graph, both defects); (b) *scoping power* — suspect
  set size as a fraction of the service. *Kill threshold: if the median
  suspect set is most of the graph, the card narrows nothing and the surface
  is not pulling its weight.*
- **E2 — graph-to-trace handoff.** For each drill with a trace, `behavior
  ingest` locates the actual divergence *inside* the card's suspect set —
  validating the "graph to narrow, telemetry to locate" claim end-to-end.
- **E3 — staleness demonstration (negative validation).** Run triage with a
  graph one commit behind a routing change and document the mis-scoping. This
  is deliberate: it converts the graph-per-deploy prerequisite (§8) from an
  assertion into evidence.
- **E4 — agent drill.** Give Claude the same staged incident with the MCP
  tools vs. with only the raw repo; compare whether its conclusions cite card
  facts and how many exploratory queries it needs. Qualitative, documented
  per drill — this is the "arm Claude" value claim, so it must be observed at
  least once before IT-4 is called done.

## 8. Operational prerequisite: graph-per-deploy

Triage interrogates the graph of the *running* commit; a stale map mis-triages.
This plan does not build the retention pipeline, but it sharpens the existing
Phase 4 (trusted zero-touch CI) requirement: the CODEOWNERS-gated job that
regenerates graphs should also archive `graph.json` per deployed commit (it is
small, canonical, and digest-bearing). Until that exists, `triage` documents
the expectation that the caller supplies the graph for the deployed SHA.
